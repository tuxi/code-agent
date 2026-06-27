package runtime

import (
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
)

// ServeRunBuilder is the conversation.RunBuilder for the HTTP/WebSocket server. It
// wraps BuildRunner and uses the per-turn publisher from TurnExecutor (which fans
// out to event store + WS subscribers).
type ServeRunBuilder struct {
	Cfg      app.Config
	MC       app.ModelConfig
	Provider model.Provider
	ToolReg  *tools.Registry
	WSReg    *WorkspaceRegistry
	PlanRef  *agent.RunnerRef // late-bound per-turn in Build()
}

// Build creates a per-turn TurnRunner that resolves skills from the session's
// workspace, merges client-registered tools, and wires plan tools + client waiter.
func (b *ServeRunBuilder) Build(ctx conversation.RuntimeContext) conversation.TurnRunner {
	// Resolve skills for the session's workspace.
	workspacePath := ctx.Session.WorkspacePath
	var skillReg *skills.Registry
	if inst, err := b.WSReg.Get(workspacePath); err == nil {
		skillReg = inst.SkillReg
	}

	runner := BuildRunner(b.Cfg, b.MC, b.Provider, b.ToolReg, skillReg, ctx.Approver, ctx.Publisher)
	if workspacePath != "" {
		runner.WorkspaceRoot = workspacePath
	}
	// Merge client-registered tools into a per-turn clone so the global registry stays unmodified.
	if len(ctx.ClientTools) > 0 {
		reg := b.ToolReg.Clone()
		for _, def := range ctx.ClientTools {
			proxy := tools.NewClientProxyTool(def.Name, def.Description, def.InputSchema)
			if err := reg.Register(proxy); err != nil {
				continue // name collision with a server tool — skip
			}
		}
		runner.Tools = reg
	}
	// Wire the plan tools, plan approver, and client tool waiter to this per-turn runner.
	b.PlanRef.R = runner
	runner.PlanApprover = ctx.PlanApprover
	runner.ClientWaiter = ctx.ClientWaiter
	runner.Stream = true // emit token_delta events for live "thinking" feel on the client
	return runner
}
