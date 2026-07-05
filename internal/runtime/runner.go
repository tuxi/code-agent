package runtime

import (
	"os"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/approve"
	"code-agent/internal/hooks"
	"code-agent/internal/model"
	"code-agent/internal/observation"
	"code-agent/internal/session"
	"code-agent/internal/settings"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
)

// userHome returns the user's home dir, or "" when it can't be resolved (which
// disables the user-scope settings layer rather than erroring).
func userHome() string {
	home, _ := os.UserHomeDir()
	return home
}

// BuildCompactor builds the summary compactor used to keep long sessions inside
// the context window. It summarizes with the same provider/model the agent is
// running, so switching models (`/use`) must rebuild it. Shared by run and repl.
// The verbatim tail is token-budgeted from the model's compaction threshold
// (P12.a), so compaction converges on a 32k local window as much as on 128k.
func BuildCompactor(cfg app.Config, mc app.ModelConfig, provider model.Provider) session.Compactor {
	return &session.LLMCompactor{
		Provider:         provider,
		ModelName:        mc.Model,
		Temperature:      mc.Temperature,
		KeepRecentTokens: cfg.CompactKeepTokens(mc),
	}
}

// BuildRunner assembles the agent.Runner shared by all entry points. The only
// things that differ are the Approver (how the user confirms a side-effecting
// tool) and the Emitter (how the event stream is rendered) — everything else
// (tools, observation, reflection, the skills nudge, compaction, the step cap) is
// identical, so it lives here and callers cannot drift apart.
func BuildRunner(cfg app.Config, mc app.ModelConfig, provider model.Provider, registry *tools.Registry, skillReg *skills.Registry, approver agent.Approver, emitter agent.Emitter, rules *approve.RuleStore) *agent.Runner {
	// Load the project settings layer once; both hooks (P11.c) and the verify
	// command (P11.b) are sourced from it.
	set := settings.Load(cfg.Workspace.Root, userHome(), os.Stderr)

	// Assign the hook runner only when non-nil, so an absent config stays a nil
	// interface (not a typed-nil that would defeat the loop's nil-safe check).
	// Hooks run `sh -c`, so on a no-subprocess host (iOS) BOTH config-layer and
	// settings-layer hooks are suppressed — never just cfg.Hooks (P11.c).
	var hook agent.ToolHook
	if cfg.Profile.AllowsSubprocess() {
		allHooks := append(append([]hooks.Hook(nil), cfg.Hooks...), set.Hooks...)
		if hr := hooks.New(allHooks, cfg.Workspace.Root); hr != nil {
			hook = hr
		}
	}

	// Pre-approve/deny tool calls matching the shared permission RuleStore
	// (Claude-style allow/deny globs, plus any "Always allow" grant made this
	// session), outermost in the approver chain so a matched rule short-circuits
	// before auto mode or the human prompt. The store is created once per
	// process/session by the caller and shared with the frontend approver (which
	// Grants into it), so a nil store just leaves the approver unchanged.
	approver = approve.Allowlisted(rules, approver)

	return &agent.Runner{
		Model:            provider,
		ModelName:        mc.Model,
		Temperature:      mc.Temperature,
		Tools:            registry,
		MaxSteps:         cfg.Agent.MaxSteps,
		MaxParallelTools: cfg.Agent.MaxParallelTools,
		Approver:         approver,
		Observer:         observation.DefaultObserver{},
		Reflector:        agent.DefaultReflector{},
		RemindSkills:     skillReg.Len() > 0,
		RemindParallel:   cfg.Agent.MaxParallelTools > 1,
		RemindHypothesis: true,
		// Verify command resolution (P11.b): the settings layer's verify block wins,
		// else the config.yaml legacy value; "auto" detects from the workspace.
		VerifyCommand: settings.ResolveVerifyFrom(set, cfg.Workspace.Root, cfg.Agent.VerifyCommand),
		PlanTools:     tools.Subset(registry, PlanModeToolNames...),
		Hook:          hook,
		Compactor:     BuildCompactor(cfg, mc, provider),
		// Tier-0 pruning shares the compactor's verbatim-tail budget (P12.c).
		CompactKeepTokens: cfg.CompactKeepTokens(mc),
		Emitter:           emitter,
		WorkspaceRoot:     cfg.Workspace.Root,
		// Client-tool lease (0 = loop's built-in 2-minute default). Raised by
		// deployments whose client tools run long (e.g. DreamAI media generation).
		ClientToolTimeout: time.Duration(cfg.Agent.ClientToolTimeoutSeconds) * time.Second,
	}
}
