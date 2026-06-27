package runtime

import (
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/hooks"
	"code-agent/internal/model"
	"code-agent/internal/observation"
	"code-agent/internal/session"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
)

// BuildCompactor builds the summary compactor used to keep long sessions inside
// the context window. It summarizes with the same provider/model the agent is
// running, so switching models (`/use`) must rebuild it. Shared by run and repl.
func BuildCompactor(mc app.ModelConfig, provider model.Provider) session.Compactor {
	return &session.LLMCompactor{
		Provider:           provider,
		ModelName:          mc.Model,
		Temperature:        mc.Temperature,
		KeepRecentMessages: 50,
	}
}

// BuildRunner assembles the agent.Runner shared by all entry points. The only
// things that differ are the Approver (how the user confirms a side-effecting
// tool) and the Emitter (how the event stream is rendered) — everything else
// (tools, observation, reflection, the skills nudge, compaction, the step cap) is
// identical, so it lives here and callers cannot drift apart.
func BuildRunner(cfg app.Config, mc app.ModelConfig, provider model.Provider, registry *tools.Registry, skillReg *skills.Registry, approver agent.Approver, emitter agent.Emitter) *agent.Runner {
	// Assign the hook runner only when non-nil, so an absent config stays a nil
	// interface (not a typed-nil that would defeat the loop's nil-safe check).
	var hook agent.ToolHook
	if hr := hooks.New(cfg.Hooks, cfg.Workspace.Root); hr != nil {
		hook = hr
	}
	return &agent.Runner{
		Model:         provider,
		ModelName:     mc.Model,
		Temperature:   mc.Temperature,
		Tools:         registry,
		MaxSteps:      cfg.Agent.MaxSteps,
		Approver:      approver,
		Observer:      observation.DefaultObserver{},
		Reflector:     agent.DefaultReflector{},
		RemindSkills:  skillReg.Len() > 0,
		PlanTools:     tools.Subset(registry, PlanModeToolNames...),
		Hook:          hook,
		Compactor:     BuildCompactor(mc, provider),
		Emitter:       emitter,
		WorkspaceRoot: cfg.Workspace.Root,
	}
}
