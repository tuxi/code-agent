package runtime

import (
	"code-agent/internal/agent"
	"code-agent/internal/credential"
	"code-agent/internal/jobs"
	"code-agent/internal/lsp"
	"code-agent/internal/mcp"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/settings"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/git"
	"code-agent/internal/tools/projectcfg"
	"code-agent/internal/tools/search"
	"code-agent/internal/tools/sessions"
	"code-agent/internal/tools/shell"
	"code-agent/internal/tools/skill"
	"code-agent/internal/tools/task"
	"code-agent/internal/tools/todo"
	"code-agent/internal/tools/webfetch"
	"code-agent/internal/tools/websearch"
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// WirePlanTools creates the plan-mode tools (enter_plan_mode, propose_plan) and
// the HITL clarification tool (ask_user), and registers them into the given
// registry. It returns a RunnerRef whose R field must be set after BuildRunner
// returns — all three tools dereference it lazily at Execute time.
func WirePlanTools(registry *tools.Registry, plansDir string) *agent.RunnerRef {
	ref := &agent.RunnerRef{}
	_ = registry.Register(agent.NewEnterPlanModeTool(ref))
	_ = registry.Register(agent.NewProposePlanTool(ref, plansDir))
	_ = registry.Register(agent.NewAskUserTool(ref))
	return ref
}

// RegisterBuiltinTools registers all built-in and config-driven tools (filesystem,
// git, shell, search, LSP, skill loader, web search/fetch, todo) into
// the registry. It does NOT register task or MCP tools — those are registered
// after the subagent's read-only toolset is frozen.
//
// jobSink, when non-nil, observes every background job's lifecycle (P8.7 Phase
// A) — pass NewJobEventSink(...) to persist job events under the job's own id
// partition, or nil for jobs invisible to the event stream (tests).
func RegisterBuiltinTools(registry *tools.Registry, cfg settings.Settings, cred credential.Resolver, skillReg *skills.Registry, root string, jobSink jobs.Sink) error {
	// Pure-Go tools that work inside an OS sandbox (no subprocess, container-local
	// filesystem and network only). Registered under every profile.
	toolList := []tools.Tool{
		filesystem.NewListFilesTool(),
		filesystem.NewReadFileTool(),
		filesystem.NewCreateFileTool(),
		filesystem.NewEditFileTool(),
		search.NewGrepTool(),
		skill.NewLoadSkillTool(skillReg, cfg.GlobalSkillsDir, filepath.Join(root, "skills")),
	}
	// LSP tools — best-effort.
	lspClient := initLSPClient(root)
	toolList = append(toolList, tools.NewLSPTools(lspClient)...)

	// Memory tools — per-workspace, lazily initialized via ExecutionContext.
	toolList = append(toolList, tools.NewMemoryTools()...)
	if searchTool := websearch.NewTool(cfg.Web, cred); searchTool != nil {
		toolList = append(toolList, searchTool)
	}
	toolList = append(toolList, webfetch.NewTool(cfg.Web), todo.NewTool(), &sessions.AnalyzeSessionsTool{})

	// Pure-Go git tools that work without a subprocess (go-git / go-gitdiff). On a
	// sandboxed host (iOS) these replace the exec-backed git tools below and add what
	// desktop gets through the shell — giving a self-contained git surface (init then
	// clone then commit / diff / apply_patch / status / log) without ever spawning git.
	// git_init is the keystone for new projects; git_clone is the entry point for
	// fetching remote repos for analysis.
	if !cfg.Profile.AllowsSubprocess() {
		for _, tool := range []tools.Tool{
			git.NewGitInitTool(),
			git.NewGitCloneTool(),
			git.NewGitPullTool(),
			git.NewGitCommitToolGoGit(),
			git.NewDiffToolGoGit(),
			git.NewApplyPatchToolGoGit(),
			git.NewGitStatusTool(),
			git.NewGitLogTool(),
		} {
			if !cfg.Agent.ToolAllowed(tool.Name()) {
				continue
			}
			if err := registry.Register(tool); err != nil {
				return err
			}
		}
	}

	// Subprocess-based tools (shell, git, gopls) are only assembled where the host
	// can fork/exec. On a sandboxed host (iOS) they would compile but fail at every
	// call, so they are left unregistered — the model never sees a tool it cannot use.
	if cfg.Profile.AllowsSubprocess() {
		// run_command and the job_* tools share one job registry, so a job_id
		// returned by a background run_command is resolvable by job_status/logs/
		// cancel/wait.
		runCmd := shell.NewRunCommandTool()
		jobReg := runCmd.Jobs
		jobReg.Sink = jobSink // before any Start (jobs.Registry.Sink contract)
		toolList = append(toolList,
			projectcfg.NewSetVerifyCommandTool(), // configure verify; needs run_command to actually run
			git.NewDiffTool(),
			git.NewApplyPatchTool(),
			git.NewGitCommitTool(),
			runCmd,
			&shell.JobStatusTool{Jobs: jobReg},
			&shell.JobLogsTool{Jobs: jobReg},
			&shell.JobCancelTool{Jobs: jobReg},
			&shell.JobWaitTool{Jobs: jobReg},
		)
	}

	for _, tool := range toolList {
		if !cfg.Agent.ToolAllowed(tool.Name()) {
			continue
		}
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

// BuildBaseRegistry registers every process-wide tool — built-ins, task, flux,
// and the plan tools — but does NOT touch MCP. It exists for serve-mode entry
// points (codeagentd, embed.Assemble), where MCP servers are resolved per
// conversation workspace by the WorkspaceRegistry rather than once from the
// process root: the returned registry is the shared base each workspace clones
// and layers its own MCP tools onto. Single-workspace entry points (run, repl,
// tui) keep using BuildRegistry, which builds on top of this.
func BuildBaseRegistry(ctx context.Context, cfg settings.Settings, mc settings.ModelConfig, provider model.Provider, cred credential.Resolver, store session.Store, root string, progress agent.Emitter) (*tools.Registry, *skills.Registry, *agent.RunnerRef, *JobEventSink, error) {
	registry := tools.NewRegistry()

	skillReg, err := skills.Load(cfg.GlobalSkillsDir, filepath.Join(root, "skills"))
	fmt.Fprintf(os.Stderr, "[registry] skills: %d loaded, %d skipped\n", skillReg.Len(), len(skillReg.Skipped))
	for label, reason := range skillReg.Skipped {
		fmt.Fprintf(os.Stderr, "[registry]   skipped %q: %s\n", label, reason)
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// Background jobs get their own event partition (P8.7 Phase A): each job's
	// lifecycle + output persists under SessionID = job id, replayable via
	// GET /v1/conversations/{job_id}/events; the bracket events are additionally
	// forwarded into the owning conversation's partition (§8.4-2). The sink is
	// returned so serve can late-bind its live resolver once the subscription
	// manager exists. Store-less builds keep jobs unobserved (nil sink).
	var jobSink *JobEventSink
	var registerSink jobs.Sink // avoid a typed-nil interface when store is nil
	if store != nil {
		jobSink = NewJobEventSink(ctx, store)
		registerSink = jobSink
	}
	if err := RegisterBuiltinTools(registry, cfg, cred, skillReg, root, registerSink); err != nil {
		return nil, nil, nil, nil, err
	}

	// Subagent (8.3): freeze the read-only subset from the built-ins ONLY — before
	// `task` and the MCP tools are registered — then register the `task` tool into
	// the PARENT. Because the subset is taken now, `task` can never be in it, so a
	// subagent cannot spawn a subagent: recursion is capped at depth 1 by
	// construction (see tools.Subset / NewSubAgent).
	if provider != nil {
		sub := NewSubAgentWithCredential(cfg, mc, provider, cred, root, registry, skillReg.PromptIndex(), store, progress, jobSink)
		if taskTool := task.NewTool(sub); cfg.Agent.ToolAllowed(taskTool.Name()) {
			if err := registry.Register(taskTool); err != nil {
				return nil, nil, nil, nil, err
			}
		}
	}

	// Flux projects the current turn's code-agent tools at execution time and no
	// longer owns a private shell registry, so the planner is available on both
	// desktop and sandboxed/iOS profiles.
	if provider != nil && cfg.Agent.ToolAllowed("plan_workflow") {
		RegisterFluxTool(registry, provider, mc.Model)
	}

	// Cross-session control plane (Phase A): list_sessions and read_session.
	// These tools are always registered — they degrade gracefully when index.db
	// is unavailable (the tool returns an error explaining the situation).
	if cfg.Agent.ToolAllowed("list_sessions") {
		if err := registry.Register(&sessions.ListSessionsTool{}); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if cfg.Agent.ToolAllowed("read_session") {
		if err := registry.Register(&sessions.ReadSessionTool{}); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if cfg.Agent.ToolAllowed("send_to_session") {
		if err := registry.Register(&sessions.SendToSessionTool{}); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if cfg.Agent.ToolAllowed("wait_sessions") {
		if err := registry.Register(&sessions.WaitSessionsTool{}); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if cfg.Agent.ToolAllowed("create_session") {
		if err := registry.Register(&sessions.CreateSessionTool{}); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if cfg.Agent.ToolAllowed("fork_session") {
		if err := registry.Register(&sessions.ForkSessionTool{}); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if cfg.Agent.ToolAllowed("check_turn") {
		if err := registry.Register(&CheckTurnTool{}); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if cfg.Agent.ToolAllowed("search_session") {
		if err := registry.Register(&sessions.SearchSession{}); err != nil {
			return nil, nil, nil, nil, err
		}
	}

	// Plan mode tools: enter_plan_mode and propose_plan. They use a RunnerRef for
	// late binding — the Runner is constructed after the registry. The returned ref
	// must be wired via planRef.R = runner after BuildRunner.
	planRef := WirePlanTools(registry, filepath.Join(root, ".codeagent", "plans"))

	return registry, skillReg, planRef, jobSink, nil
}

// BuildRegistry builds the base registry (BuildBaseRegistry) and then connects
// any configured MCP servers — registering their tools into the SAME registry,
// so remote tools are gated and observed exactly like built-ins. Shared by the
// single-workspace entry points (run, repl, tui), where the process root IS the
// workspace and one MCP config for the whole process is the correct semantics.
// Serve-mode entry points use BuildBaseRegistry + workspace-scoped MCP instead
// (see WorkspaceRegistry.EnableMCP). The returned skills registry feeds both the
// load_skill tool and the system-prompt index, so the index the model sees and
// the bodies it can load stay in sync. The returned Manager owns the MCP
// subprocesses; the caller must Close it.
func BuildRegistry(ctx context.Context, cfg settings.Settings, mc settings.ModelConfig, provider model.Provider, store session.Store, root string, progress agent.Emitter) (*tools.Registry, *skills.Registry, *mcp.Manager, *agent.RunnerRef, *JobEventSink, error) {
	registry, skillReg, planRef, jobSink, err := BuildBaseRegistry(ctx, cfg, mc, provider, cfg.CredentialResolver(nil), store, root, progress)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	// MCP tools are registered AFTER the built-ins, so they appear after them in
	// the advertised list (the Registry preserves registration order). A server
	// that fails to start is skipped inside Connect; a name collision surfaces here
	// as a registration error.
	//
	// Only stdio servers spawn a subprocess, so on a sandboxed host (iOS, where the
	// OS forbids fork/exec) we drop those and connect only http/sse servers, which
	// need no local process — that is the only MCP available on iOS. On a full
	// desktop host all servers connect.
	mgr := mcp.NewManager(McpTraceWriter())
	servers := cfg.MCP.Servers
	if !cfg.Profile.AllowsSubprocess() {
		servers = mcp.RemoteServers(servers)
	}
	if n := len(servers); n > 0 {
		fmt.Fprintf(os.Stderr, "[mcp] connecting to %d server(s)…\n", n)
	}
	if err := mgr.Connect(ctx, servers); err != nil {
		mgr.Close()
		return nil, nil, nil, nil, nil, err
	}
	for _, tool := range mgr.Tools() {
		if !cfg.Agent.ToolAllowed(tool.Name()) {
			continue
		}
		if err := registry.Register(tool); err != nil {
			mgr.Close()
			return nil, nil, nil, nil, nil, fmt.Errorf("register mcp tool: %w", err)
		}
	}

	return registry, skillReg, mgr, planRef, jobSink, nil
}

// initLSPClient detects the project language and returns a Client.
// Initialization runs in the background and never blocks startup.
// Returns nil when no language is recognised or root is empty
// (daemon mode — LSP needs a concrete workspace root for file paths).
func initLSPClient(root string) *lsp.Client {
	if root == "" {
		return nil
	}
	client, err := lsp.DetectLanguage(root)
	if err != nil {
		return nil
	}
	// Start initialization in the background — never block startup.
	// Tools check client.Ready() and gracefully degrade until it returns true.
	go func() {
		if err := client.Initialize(context.Background(), root); err != nil {
			fmt.Fprintf(os.Stderr, "[lsp] %s init failed: %v (LSP disabled for this session)\n", client.Language(), err)
		}
	}()
	return client
}
