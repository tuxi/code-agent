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
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"

	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/repos"
	"code-agent/internal/runtime"
	"code-agent/internal/server"
)

// defaultCapabilities is the capability set advertised in the WebSocket hello
// handshake. Keep in sync with cmd/codeagent/serve.go.
var defaultCapabilities = []string{
	"streaming",
	"thinking",
	"tool_streaming",
	"plan_mode",
	"subagents",
	"session_resume",
	"client_tool_execution",
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

	addr := "127.0.0.1:8797"
	if len(args) > 0 {
		addr = args[0]
	}

	cfg, err := app.LoadConfig("config.yaml")
	if err != nil {
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

	mc, err := cfg.SelectModel(modelName)
	if err != nil {
		return err
	}

	provider, err := runtime.BuildProvider(mc, cfg.Provider, nil)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	home, err := os.UserHomeDir()
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
	toolReg, _, planRef, jobSink, err := runtime.BuildBaseRegistry(ctx, cfg, mc, provider, cfg.CredentialResolver(nil), telemetryStore, "", nil)
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
	rb := runtime.NewServeRunBuilder(cfg, mc, provider, cfg.CredentialResolver(nil), toolReg, wsReg, planRef)
	executor := conversation.NewTurnExecutor(repo, eventStore, active, subs, rb)
	maxConcurrentTurns := cfg.RuntimeMaxConcurrentTurns()
	executor.SetTurnScheduler(conversation.NewTurnScheduler(maxConcurrentTurns))
	executor.SetTitleGenerator(conversation.NewLLMTitleGenerator(provider, mc.Model))
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
	capabilities := append([]string(nil), defaultCapabilities...)
	if cloneService != nil {
		capabilities = append(capabilities, "public_git_clone_v1")
	}
	handler := server.NewMux(repo, eventStore, executor, server.MuxOptions{
		ServerName:          "codeagentd/" + mc.Model,
		Capabilities:        capabilities,
		CloneService:        cloneService,
		Granter:             rb.Rules(),
		WorkspaceReloader:   wsReg.ReloadWorkspace,
		Prompts:             wsReg,
		CredentialStore:     executor.SetSessionCredential,
		RuntimeCapabilities: runtimeCapabilities,
		ManagedWorktrees:    managedWorktrees,
	})

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Printf("codeagentd serve — http://%s  (model: %s, cwd: %s, projects: %s)\n", addr, mc.Model, cwd, projectsRoot)
	fmt.Println("  GET  /healthz")
	fmt.Println("  GET  /v1/conversations")
	fmt.Println("  POST  /v1/conversations            {\"workspace_path\":\"...\"}  -> {\"id\":\"...\"}")
	fmt.Println("  PATCH /v1/conversations/{id}        {\"name\":\"...\"}")
	fmt.Println("  GET   /v1/conversations/{id}/stream   (WebSocket)")

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	fmt.Fprintln(os.Stderr, "codeagentd: stopped")
	return nil
}
