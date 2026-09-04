// Command codeagentd is the daemon-mode entry point for the CodeAgent runtime
// server. It starts an HTTP + WebSocket server with no terminal/TUI dependencies,
// designed to be launched from an IDE (GoLand), systemd, launchd, or Docker.
//
// Usage:
//
//	codeagentd [--model NAME] [addr]   default addr: 127.0.0.1:8797
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/automation"
	"code-agent/internal/buildinfo"
	"code-agent/internal/controlplane"
	"code-agent/internal/conversation"
	"code-agent/internal/credential"
	"code-agent/internal/gitworkspace"
	"code-agent/internal/model"
	"code-agent/internal/repos"
	"code-agent/internal/runtime"
	"code-agent/internal/server"
	"code-agent/internal/settings"
)

// defaultCapabilities is the capability set advertised in the WebSocket hello
// handshake. Keep in sync with cmd/codeagent/serve.go.
var defaultCapabilities = []string{
	"streaming",
	"thinking",
	"reasoning_streaming",
	"tool_streaming",
	"plan_mode",
	"subagents",
	"child_streaming",
	"session_resume",
	"client_tool_execution",
	"workflow_execution",
	"conversation_fork_v1",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	modelName, args := runtime.ExtractModelFlag(args)

	var portFile string
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch {
		case strings.HasPrefix(args[0], "--port-file="):
			portFile = strings.TrimPrefix(args[0], "--port-file=")
		default:
			return fmt.Errorf("unknown flag: %s", args[0])
		}
		args = args[1:]
	}

	addr := "0.0.0.0:8797"
	if len(args) > 0 {
		addr = args[0]
	}

	root, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	cfg, _ := runtime.LoadSettingsWithTrust(context.Background(), root, home, nil, runtime.TrustAlways, os.Stderr)
	// Mutable injected resolver (A2): POST /v1/secrets updates it, and every
	// credential chain (catalog probing + actual calls) reads it — so a host
	// can push provider keys to a running daemon without restarting.
	mutableResolver := credential.NewMutableResolver()
	var err error

	// Shared credential share (Claude-style): the host app writes provider
	// keys to ~/.codeagent/secrets.json. Load them so catalog probing and
	// provider calls see app-managed keys even when no env var is set (the
	// CLI does the same at startup). A POST /v1/secrets push (mutableResolver)
	// wins over the file, which wins over env (CredentialResolver appends the
	// env resolver after this injected layer). A missing/empty file yields nil.
	secretsResolver, err := credential.SecretsFile{}.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not load ~/.codeagent/secrets.json:", err)
	}
	baseResolver := credential.Resolver(mutableResolver)
	if secretsResolver != nil {
		baseResolver = &credential.ChainResolver{Resolvers: []credential.Resolver{mutableResolver, secretsResolver}}
	}
	auth, err := server.ResolveExternalServerAuth(cfg.Server)
	if err != nil {
		return err
	}
	if err := server.ValidateExternalDeployment(addr, cfg.Server, auth); err != nil {
		return err
	}
	// MCP servers come from Claude-compatible `.mcp.json` files resolved PER
	// CONVERSATION WORKSPACE (layered local > project > user, matching Claude
	// Code), not from the daemon's launch directory — see WorkspaceRegistry.
	// EnableMCP below. Set CODEAGENT_MCP_INHERIT_CLAUDE=1 to also inherit
	// user-scope servers from an existing ~/.claude.json.
	inheritClaude := os.Getenv("CODEAGENT_MCP_INHERIT_CLAUDE") == "1"
	if cfg.GlobalSkillsDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.GlobalSkillsDir = filepath.Join(home, ".codeagent", "skills")
		}
	}
	if cfg.StoreFactory != nil {
		runtime.StoreFactory = cfg.StoreFactory
	}

	var mc settings.ModelConfig
	var provider model.Provider
	if len(cfg.Models) > 0 {
		mc, err = app.SelectModel(modelName, cfg)
		if err != nil {
			return err
		}
		provider, err = runtime.BuildProvider(mc, cfg.Provider, cfg.CredentialResolver(baseResolver))
		if err != nil {
			return err
		}
	} else if modelName != "" {
		return runtime.ModelNotConfiguredError{}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	home, err = os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home for projects root: %w", err)
	}
	projectsRoot := filepath.Join(home, "Documents")
	cloneStateDir := filepath.Join(home, ".codeagent", "clone")
	cloneService, cloneErr := repos.NewService(projectsRoot, cloneStateDir)
	if cloneErr != nil {
		fmt.Fprintf(os.Stderr, "codeagentd: public git clone disabled: %v\n", cloneErr)
	} else {
		defer cloneService.Close()
	}

	// The daemon's global telemetry store uses CWD as its identity key. This
	// means codeagentd launched from /path/to/project-a naturally isolates its
	// sessions from codeagentd launched from /path/to/project-b — the same
	// per-project hashing that the store has always used. Before Phase 3 the
	// daemon passed cfg.Workspace.Root (= ".") which resolved to the same CWD.
	// After Phase 3 (cfg.Workspace removed) we pass CWD explicitly.
	//
	// The clone-target directory is the only path that MUST be CWD-independent:
	// projectsRoot is the user's Documents directory, not the launch directory.
	cwd, _ := os.Getwd()
	telemetryStore, err := runtime.OpenStore(cwd)
	if err != nil {
		return err
	}
	defer telemetryStore.Close()
	runtime.AttachObserver(provider, telemetryStore, ctx)

	// Build the shared BASE tool registry once: built-ins only, no MCP. Each
	// workspace clones it and layers its own MCP tools on top (EnableMCP), so the
	// daemon's launch directory never decides which MCP tools a conversation sees.
	// root for skills: "" means no project-local skills from the daemon itself;
	// workspace-scoped skills are loaded per instance by WorkspaceRegistry.
	toolReg, _, planRef, jobSink, err := runtime.BuildBaseRegistry(ctx, cfg, mc, provider, cfg.CredentialResolver(baseResolver), telemetryStore, "", nil)
	if err != nil {
		return err
	}

	wsReg := runtime.NewWorkspaceRegistry(cfg.GlobalSkillsDir)
	wsReg.EnableMCP(ctx, toolReg, cfg, nil, inheritClaude)
	defer wsReg.Close()

	// Execution Model components.
	repo := conversation.NewSQLiteRepository(
		telemetryStore,
		mc.ContextWindow,
		cfg.CompactThreshold(mc),
		mc.Model,
		"", // desktop: absolute workspace paths are stable; no re-anchoring
		func(workspacePath string) string {
			inst, err := wsReg.Get(workspacePath)
			if err != nil {
				return ""
			}
			return inst.SkillReg.PromptIndex()
		},
	)

	eventStore := &conversation.StoreEventAdapter{Store: telemetryStore}
	active := conversation.NewActiveTurnRegistry()
	subs := conversation.NewSubscriptionManager()
	rb := runtime.NewServeRunBuilder(cfg, mc, provider, cfg.CredentialResolver(baseResolver), toolReg, wsReg, planRef)
	executor := conversation.NewTurnExecutor(repo, eventStore, active, subs, rb)
	executor.SetAssetRefReleaseService(rb)
	maxConcurrentTurns := cfg.RuntimeMaxConcurrentTurns()
	executor.SetTurnScheduler(conversation.NewTurnScheduler(maxConcurrentTurns))
	if provider != nil {
		executor.SetTitleGenerator(conversation.NewLLMTitleGenerator(provider, mc.Model))
	}
	_ = executor.ReconcileInterrupted(ctx)

	// Automation engine (T1-T2): open the process-wide automation store, wire the
	// dispatcher (standalone creates a new conversation, chat returns to a fixed
	// session), and start the scheduler loop. The store is injected into the
	// runner builder (so the automation tools work in every conversation) and into
	// the mux (so the /v1/automations control-plane endpoints work).
	stateDir, stateDirErr := runtime.StateDir()
	if stateDirErr != nil {
		return stateDirErr
	}
	var automationStore automation.Store
	var dispatcher *automation.TurnDispatcher
	automationStore, automationErr := automation.OpenStore(filepath.Join(stateDir, "automations.db"))
	if automationErr != nil {
		fmt.Fprintf(os.Stderr, "codeagentd: automation store disabled: %v\n", automationErr)
	} else {
		defer automationStore.Close()
		dispatcher = automation.NewTurnDispatcher(&daemonAutomationAdapter{exec: executor, repo: repo}, &daemonAutomationAdapter{exec: executor, repo: repo})
		scheduler := automation.NewScheduler(automationStore, dispatcher, 0)
		if skipped, err := scheduler.Reconcile(ctx); err == nil && skipped > 0 {
			fmt.Fprintf(os.Stderr, "codeagentd: automation reconcile skipped %d overdue firings\n", skipped)
		}
		scheduler.Start()
		defer scheduler.Stop()
		rb.SetAutomationStore(automationStore)
	}
	managedWorktrees, worktreeReport, worktreeErr := runtime.ConfigureManagedWorktrees(ctx, telemetryStore, repo, executor, true)
	if worktreeErr != nil {
		fmt.Fprintf(os.Stderr, "codeagentd: managed worktrees disabled: %v\n", worktreeErr)
	} else if managedWorktrees != nil && (len(worktreeReport.Issues) > 0 || len(worktreeReport.Orphans) > 0) {
		fmt.Fprintf(os.Stderr, "codeagentd: managed worktree reconciliation: issues=%d orphans=%d missing=%d\n", len(worktreeReport.Issues), len(worktreeReport.Orphans), len(worktreeReport.Missing))
	}
	// Job bracket events reach the owning conversation's live subscribers (P8.7
	// §8.4-2) — persisted copies are already handled inside the sink.
	if jobSink != nil {
		jobSink.SetLiveResolver(subs.Emitter)
	}

	runtimeCapabilities := server.ConfiguredRuntimeCapabilities(maxConcurrentTurns)
	runtimeCapabilities.ManagedWorktree = managedWorktrees != nil
	gitBranches := gitworkspace.New(cwd, executor, managedWorktrees)
	capabilities := append([]string(nil), defaultCapabilities...)
	capabilities = append(capabilities, gitworkspace.Capability)
	if cloneService != nil {
		capabilities = append(capabilities, "public_git_clone_v1")
	}
	runtimeInfo, runtimeModels, err := server.BuildRuntimeContract(
		cfg, cwd, cfg.Server.DisplayName, server.RuntimeProfileHeadless, baseResolver,
	)
	if err != nil {
		return err
	}
	stateDir, err = runtime.StateDir()
	if err != nil {
		return err
	}
	// Sharing state is per-workspace (same directory as the project's session
	// store and runtime-server-state.json) so its server_id binding always
	// matches the project-local server identity. A single global file would be
	// claimed by whichever workspace started a daemon first and reject every
	// other workspace ("sharing state belongs to another server").
	sharingDir, err := runtime.RuntimeStateDir(root)
	if err != nil {
		return fmt.Errorf("resolve sharing state directory: %w", err)
	}
	sharing, err := server.OpenDaemonRuntimeSharing(sharingDir, runtimeInfo.ServerID, runtimeInfo.DisplayName)
	if err != nil {
		return fmt.Errorf("open runtime sharing state: %w", err)
	}
	defer sharing.Stop()
	owner, err := controlplane.NewManager(stateDir, runtimeInfo.ServerID, repo, executor, controlplane.Config{})
	if err != nil {
		return err
	}
	owner.SetTarget(controlplane.NewRuntimeTarget(executor, eventStore, repo, managedWorktrees))
	rb.SetSessionControl(owner)
	rb.SetWorkflowRunner(runtime.NewWorkflowRunner(owner, wsReg))
	rb.SetSessionTrace(runtime.NewSessionTraceFunc())
	// Workflow-mode automations (workflow_ref) trigger a template directly.
	dispatcher.SetWorkflowRunner(runtime.NewAutomationWorkflowRunner(runtime.NewHeadlessRuntime(owner, wsReg)))
	runtime.SetFluxExternalResolver(owner)
	if err := owner.Start(ctx); err != nil {
		_ = owner.Close()
		return err
	}
	defer owner.Close()
	ownerIdentity := owner.Identity()
	fmt.Fprintf(os.Stderr, "[control-plane] owner ready instance=%s endpoint=%s protocol=%d\n", ownerIdentity.InstanceID, ownerIdentity.Endpoint, ownerIdentity.ProtocolVersion)
	var cfgMu sync.RWMutex
	providerStore := server.NewProviderStore(settings.UserPath(home), nil)
	var reloadMu sync.Mutex
	reloadSettings := func() error {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		next, _ := runtime.LoadSettingsWithTrust(context.Background(), root, home, nil, runtime.TrustAlways, os.Stderr)
		nextMC, err := app.SelectModel(modelName, next)
		if err != nil {
			return err
		}
		nextCred := next.CredentialResolver(baseResolver)
		nextProvider, err := runtime.BuildProvider(nextMC, next.Provider, nextCred)
		if err != nil {
			return err
		}
		rb.ReconfigureSettings(next, nextMC, nextProvider, nextCred)
		cfgMu.Lock()
		cfg = next
		cfgMu.Unlock()
		return nil
	}
	providerStore.SetReconfigure(reloadSettings)
	settingsWatcher, err := app.Watch(settings.UserPath(home), 250*time.Millisecond, reloadSettings, func(err error) {
		fmt.Fprintf(os.Stderr, "[settings] daemon reload deferred: %v\n", err)
	})
	if err != nil {
		return err
	}
	defer settingsWatcher.Close()
	handler := server.NewMux(repo, eventStore, executor, server.MuxOptions{
		Sharing:       sharing,
		ServerName:    buildinfo.ServerName(),
		RuntimeInfo:   runtimeInfo,
		RuntimeModels: runtimeModels,
		RuntimeModelsBuilder: func() server.RuntimeModelCatalog {
			// A2: live rebuild with the current injected credentials so a
			// POST /v1/secrets makes models available without a restart.
			cfgMu.RLock()
			current := cfg
			cfgMu.RUnlock()
			return server.BuildRuntimeModelCatalog(current, baseResolver)
		},
		InjectSecrets: func(targets map[credential.Target]credential.Credential) error {
			mutableResolver.MergeAll(targets)
			return nil
		},
		ReloadSettings:    reloadSettings,
		ServerAuth:        auth,
		Capabilities:      capabilities,
		CloneService:      cloneService,
		Granter:           rb.Rules(),
		Providers:         providerStore,
		Permissions:       server.NewPermissionStore(home),
		WorkspaceReloader: wsReg.ReloadWorkspace,
		Prompts:           wsReg,
		CapabilityResolver: func(ctx context.Context) []string {
			cfgMu.RLock()
			current := cfg
			cfgMu.RUnlock()
			persistence, ok := repo.(conversation.UserAssetsPersistenceCapability)
			if ok && persistence.SupportsUserAssetsPersistence() && rb.ImageInputCapability(ctx, current.CredentialResolver(baseResolver)) {
				return []string{"image_input"}
			}
			return nil
		},
		SessionReady: func(ctx context.Context, sessionID string) {
			if err := owner.Heartbeat(context.WithoutCancel(ctx)); err != nil {
				fmt.Fprintf(os.Stderr, "[control-plane] session ready reconcile: %v\n", err)
			}
			_, _ = executor.RecoverSessionTurnInputs(context.WithoutCancel(ctx), sessionID)
			go executor.FlushAssetRefReleases(context.WithoutCancel(ctx), cfg.CredentialResolver(baseResolver))
		},
		OwnershipChanged: func(ctx context.Context) {
			if err := owner.Heartbeat(context.WithoutCancel(ctx)); err != nil {
				fmt.Fprintf(os.Stderr, "[control-plane] ownership reconcile: %v\n", err)
			}
		},
		RuntimeCapabilities:    runtimeCapabilities,
		ManagedWorktrees:       managedWorktrees,
		GitBranches:            gitBranches,
		SessionForks:           owner,
		WorkflowSnapshot:       runtime.NewWorkflowSnapshotFunc(),
		WorkflowList:           runtime.NewWorkflowListFunc(),
		WorkflowDetail:         runtime.NewWorkflowDetailFunc(),
		WorkflowSnapshotByTask: runtime.NewWorkflowSnapshotByTaskFunc(),
		WorkflowRun:            runtime.NewHeadlessRuntime(owner, wsReg).SubmitHeadlessRun,
		WorkflowSaveTemplate:   runtime.NewSaveTemplateFunc(),
		WorkflowResume:         runtime.NewHeadlessRuntime(owner, wsReg).ResumeRun,
		AutomationStore:        automationStore,
	})
	sharing.SetCoreHandler(handler)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	actualPort := lis.Addr().(*net.TCPAddr).Port

	// Write the actual port to the port-file before serving, so the
	// host process can discover the ephemeral port reliably.
	if portFile != "" {
		if err := os.WriteFile(portFile, []byte(fmt.Sprint(actualPort)), 0644); err != nil {
			_ = lis.Close()
			return fmt.Errorf("write port-file %s: %w", portFile, err)
		}
	}

	srv := &http.Server{Handler: handler}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	scheme := "http"
	if cfg.Server.TLSCertificate != "" {
		scheme = "https"
	}
	fmt.Printf("codeagentd serve — %s://127.0.0.1:%d  (model: %s, cwd: %s, projects: %s)\n", scheme, actualPort, mc.Model, cwd, projectsRoot)
	fmt.Println("  GET  /healthz")
	fmt.Println("  GET  /v1/conversations")
	fmt.Println("  POST  /v1/conversations            {\"workspace_path\":\"...\"}  -> {\"id\":\"...\"}")
	fmt.Println("  POST  /v1/conversations/{id}/forks {\"request_id\":\"...\"} -> {\"id\":\"...\"}")
	fmt.Println("  PATCH /v1/conversations/{id}        {\"name\":\"...\"}")
	fmt.Println("  GET   /v1/conversations/{id}/stream   (WebSocket)")

	serve := func() error { return srv.Serve(lis) }
	if cfg.Server.TLSCertificate != "" {
		serve = func() error {
			return srv.ServeTLS(lis, cfg.Server.TLSCertificate, cfg.Server.TLSPrivateKey)
		}
	}
	if err := serve(); err != nil && err != http.ErrServerClosed {
		return err
	}
	fmt.Fprintln(os.Stderr, "codeagentd: stopped")
	return nil
}

// daemonAutomationAdapter adapts the conversation executor + repository to the
// automation package's narrow TurnSubmitter/ConversationCreator seams. It lives
// here (not in internal/automation) so the automation package stays free of the
// conversation import (which would create an import cycle through tools).
type daemonAutomationAdapter struct {
	exec *conversation.TurnExecutor
	repo conversation.ConversationRepository
}

func (a *daemonAutomationAdapter) Submit(ctx context.Context, sessionID, prompt, model string, perm automation.Perm) (string, error) {
	// Apply the per-task permission context (WorkBuddy's per-task "Full access" /
	// "Connectors without confirmation"). A declared tier is applied as a
	// per-turn override through the normal ModeApprover — deny rules and
	// protected paths still hold, unlike the old replace-the-approver approach —
	// and cleared after the turn so it never leaks into a later interactive turn.
	// "" inherits the workspace's tier. Connectors auto-approve only the named
	// MCP servers.
	mode := perm.PermissionMode
	if normalized, ok := automation.NormalizePermissionMode(mode); ok {
		mode = normalized
	} else {
		mode = "" // unknown value: be conservative, inherit the workspace tier
	}
	if mode != "" {
		a.exec.SetApprovalMode(sessionID, mode)
	}
	if len(perm.Connectors) > 0 {
		a.exec.SetApprover(sessionID, connectorApprover{connectors: perm.Connectors})
	}
	_, err := a.exec.Execute(ctx, sessionID, prompt, model, "")
	// Clear the injected tier override and connector approver so neither leaks
	// into later turns of the same session (chat/reuse mode).
	a.exec.SetApprovalMode(sessionID, "")
	a.exec.SetApprover(sessionID, nil)
	if err != nil {
		// Record the failure into the session so the user can open the standalone
		// conversation and see why the automation failed (the conversation is kept,
		// not rolled back — see dispatcher).
		a.recordFailure(ctx, sessionID, prompt, err)
		return "", err
	}
	// Return the conversation id (not the turn id): the scheduler records it as
	// the run's session and, in reuse mode, persists it as the automation's
	// session_id so later firings return to the same conversation.
	return sessionID, nil
}

// recordFailure appends the automation prompt and the failure reason to the
// session, so a failed standalone firing leaves a visible, debuggable
// conversation instead of an empty one.
func (a *daemonAutomationAdapter) recordFailure(ctx context.Context, sessionID, prompt string, err error) {
	sess, loadErr := a.repo.Load(ctx, sessionID)
	if loadErr != nil {
		return
	}
	sess.Messages = append(sess.Messages,
		model.Message{Role: model.RoleUser, Content: prompt},
		model.Message{Role: model.RoleAssistant, Content: "⚠️ 自动化任务执行失败：" + err.Error()},
	)
	_ = a.repo.Save(ctx, sess)
}

func (a *daemonAutomationAdapter) CreateConversation(ctx context.Context, workspacePath string) (string, error) {
	sess, err := a.repo.Create(ctx, workspacePath, "")
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

func (a *daemonAutomationAdapter) DeleteConversation(ctx context.Context, sessionID string) error {
	return a.repo.Delete(ctx, sessionID)
}

func (a *daemonAutomationAdapter) ConversationExists(ctx context.Context, sessionID string) (bool, error) {
	if _, err := a.repo.Load(ctx, sessionID); err != nil {
		// A load failure (e.g. session deleted) means the conversation is gone;
		// reuse mode will create a fresh one.
		return false, nil
	}
	return true, nil
}

// connectorApprover auto-approves tool calls whose name is prefixed by an
// authorized connector (MCP server), and defers everything else (Ask) so the
// session default approval still applies to non-connector tools. It is only
// installed for firings that declare connectors; a firing without connectors
// runs through the plain tier chain.
type connectorApprover struct {
	connectors []string
}

func (c connectorApprover) Approve(toolName string, _ json.RawMessage) agent.Verdict {
	for _, conn := range c.connectors {
		if strings.HasPrefix(toolName, conn+"__") || toolName == conn {
			return agent.VerdictAllow
		}
	}
	return agent.VerdictAsk
}
