package runtime

import (
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/mcp"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/git"
	projectgraph "code-agent/internal/tools/project_graph"
	"code-agent/internal/tools/search"
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
// registers them into the given registry. It returns a RunnerRef whose R field
// must be set after BuildRunner returns.
func WirePlanTools(registry *tools.Registry, plansDir string) *agent.RunnerRef {
	ref := &agent.RunnerRef{}
	registry.Register(agent.NewEnterPlanModeTool(ref))
	registry.Register(agent.NewProposePlanTool(ref, plansDir))
	return ref
}

// RegisterBuiltinTools registers all built-in and config-driven tools (filesystem,
// git, shell, search, project_graph, skill loader, web search/fetch, todo) into
// the registry. It does NOT register task or MCP tools — those are registered
// after the subagent's read-only toolset is frozen.
func RegisterBuiltinTools(registry *tools.Registry, cfg app.Config, skillReg *skills.Registry) error {
	// Pure-Go tools that work inside an OS sandbox (no subprocess, container-local
	// filesystem and network only). Registered under every profile.
	toolList := []tools.Tool{
		filesystem.NewListFilesTool(),
		filesystem.NewReadFileTool(),
		filesystem.NewCreateFileTool(),
		filesystem.NewEditFileTool(),
		search.NewGrepTool(),
		skill.NewLoadSkillTool(skillReg),
		websearch.NewTool(cfg.Web),
		webfetch.NewTool(cfg.Web),
		todo.NewTool(),
	}

	// Pure-Go git tools that work without a subprocess (go-git / go-gitdiff). On a
	// sandboxed host (iOS) these replace the exec-backed git tools below and add what
	// desktop gets through the shell — giving a self-contained git surface (init then
	// commit / diff / apply_patch / status / log) without ever spawning git. git_init
	// is the keystone: an iOS app folder starts un-versioned, so the rest depend on it.
	if !cfg.Profile.AllowsSubprocess() {
		for _, tool := range []tools.Tool{
			git.NewGitInitTool(),
			git.NewGitCommitToolGoGit(),
			git.NewDiffToolGoGit(),
			git.NewApplyPatchToolGoGit(),
			git.NewGitStatusTool(),
			git.NewGitLogTool(),
		} {
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
		// returned by a background run_command is resolvable by job_status/logs/cancel.
		runCmd := shell.NewRunCommandTool()
		jobReg := runCmd.Jobs
		toolList = append(toolList,
			projectgraph.NewProjectGraphTool(),
			git.NewDiffTool(),
			git.NewApplyPatchTool(),
			git.NewGitCommitTool(),
			runCmd,
			&shell.JobStatusTool{Jobs: jobReg},
			&shell.JobLogsTool{Jobs: jobReg},
			&shell.JobCancelTool{Jobs: jobReg},
		)
	}

	for _, tool := range toolList {
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

// BuildRegistry registers the model-facing tool set, loads the skills registry,
// and connects any configured MCP servers — registering their tools into the
// SAME registry, so remote tools are gated and observed exactly like built-ins.
// Shared by run, repl, and tui. The returned skills registry feeds both the
// load_skill tool (here) and the system-prompt index (the session builder), so
// the index the model sees and the bodies it can load stay in sync. The returned
// Manager owns the MCP subprocesses; the caller must Close it.
func BuildRegistry(ctx context.Context, cfg app.Config, mc app.ModelConfig, provider model.Provider, store session.Store, progress agent.Emitter) (*tools.Registry, *skills.Registry, *mcp.Manager, *agent.RunnerRef, error) {
	root := cfg.Workspace.Root
	registry := tools.NewRegistry()

	skillReg, err := skills.Load(filepath.Join(root, "skills"))
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if err := RegisterBuiltinTools(registry, cfg, skillReg); err != nil {
		return nil, nil, nil, nil, err
	}

	// Subagent (8.3): freeze the read-only subset from the built-ins ONLY — before
	// `task` and the MCP tools are registered — then register the `task` tool into
	// the PARENT. Because the subset is taken now, `task` can never be in it, so a
	// subagent cannot spawn a subagent: recursion is capped at depth 1 by
	// construction (see tools.Subset / NewSubAgent).
	sub := NewSubAgent(cfg, mc, provider, root, registry, skillReg.PromptIndex(), store, progress)
	if err := registry.Register(task.NewTool(sub)); err != nil {
		return nil, nil, nil, nil, err
	}

	// MCP tools are registered AFTER the built-ins, so they appear after them in
	// the advertised list (the Registry preserves registration order). A server
	// that fails to start is skipped inside Connect; a name collision surfaces
	// here as a registration error.
	// MCP servers and flux both rely on subprocesses (MCP launches each server over
	// stdio; flux wires a shell tool into its workflow registry), so on a sandboxed
	// host they are skipped. The Manager is still created so the caller's Close is a
	// safe no-op.
	mgr := mcp.NewManager(McpTraceWriter())
	if cfg.Profile.AllowsSubprocess() {
		if n := len(cfg.MCP.Servers); n > 0 {
			fmt.Fprintf(os.Stderr, "[mcp] connecting to %d server(s)…\n", n)
		}
		if err := mgr.Connect(ctx, cfg.MCP.Servers); err != nil {
			mgr.Close()
			return nil, nil, nil, nil, err
		}
		for _, tool := range mgr.Tools() {
			if err := registry.Register(tool); err != nil {
				mgr.Close()
				return nil, nil, nil, nil, fmt.Errorf("register mcp tool: %w", err)
			}
		}

		// Flux v3: Tool embedding — register plan_workflow as a native tool.
		// Uses the same process and LLM creds as code-agent (mc, resolved from config.yaml).
		RegisterFluxTool(registry, mc, nil) // mc → reuse resolved LLM creds; nil → in-memory stores
	}

	// Plan mode tools: enter_plan_mode and propose_plan. They use a RunnerRef for
	// late binding — the Runner is constructed after the registry. The returned ref
	// must be wired via planRef.R = runner after BuildRunner.
	planRef := WirePlanTools(registry, filepath.Join(root, ".codeagent", "plans"))

	return registry, skillReg, mgr, planRef, nil
}
