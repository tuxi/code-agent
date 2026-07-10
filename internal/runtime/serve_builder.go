package runtime

import (
	"fmt"
	"os"
	"sync"

	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/approve"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
)

// ServeRunBuilder is the conversation.RunBuilder for the HTTP/WebSocket server. It
// wraps BuildRunner and uses the per-turn publisher from TurnExecutor (which fans
// out to event store + WS subscribers).
//
// MC and Provider are guarded by mu so Reconfigure can hot-swap the model/creds
// (v1.2 §3.3) without racing an in-flight Build. An in-flight turn keeps the
// runner it was built with; the swap lands at the next Build, i.e. the next turn
// boundary — the same guarantee the TUI's /use already relies on.
type ServeRunBuilder struct {
	Cfg     app.Config
	ToolReg *tools.Registry
	WSReg   *WorkspaceRegistry
	PlanRef *agent.RunnerRef // late-bound per-turn in Build()

	// rules is the process-wide permission store, created once so a grant persists
	// across turns. Interactive "Always allow" over the wire is not wired yet, but
	// rules loaded from config + settings files are enforced here.
	rules *approve.RuleStore

	mu       sync.RWMutex
	mc       app.ModelConfig
	provider model.Provider
}

// NewServeRunBuilder constructs the builder with the initial model + provider.
func NewServeRunBuilder(cfg app.Config, mc app.ModelConfig, provider model.Provider, toolReg *tools.Registry, wsReg *WorkspaceRegistry, planRef *agent.RunnerRef) *ServeRunBuilder {
	return &ServeRunBuilder{
		Cfg: cfg, ToolReg: toolReg, WSReg: wsReg, PlanRef: planRef,
		rules: approve.NewRuleStore(cfg.Workspace.Root, cfg.Permissions.Allow, cfg.Permissions.Deny),
		mc:    mc, provider: provider,
	}
}

// Rules exposes the process-wide permission store so the server layer can share
// it with the RemoteApprover (which grants a client's "always allow" into it) —
// the same instance the per-turn allowlist reads, so a grant takes effect at once.
func (b *ServeRunBuilder) Rules() *approve.RuleStore { return b.rules }

// Reconfigure hot-swaps the model config and provider used by future turns
// (v1.2 §3.3). It does not touch the listener or any in-flight turn.
func (b *ServeRunBuilder) Reconfigure(mc app.ModelConfig, provider model.Provider) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mc = mc
	b.provider = provider
}

// Build creates a per-turn TurnRunner that resolves skills from the session's
// workspace, merges client-registered tools, and wires plan tools + client waiter.
func (b *ServeRunBuilder) Build(ctx conversation.RuntimeContext) conversation.TurnRunner {
	b.mu.RLock()
	mc, baseProvider := b.mc, b.provider
	cfg := b.Cfg
	b.mu.RUnlock()

	// Per-turn model selection: if the client specified a model name, first
	// try to match a config profile. When no profile matches, treat it as a
	// direct wire model string (for Gateway — the Gateway chooses the provider).
	if ctx.Model != "" {
		if altMC, err := cfg.SelectModel(ctx.Model); err == nil {
			mc = altMC
		} else {
			// Not a config profile — forward as-is to the provider.
			mc.Model = ctx.Model
		}
	}

	// If the session has a per-session credential (server mode — JWT from
	// Authorization header), build a provider that uses it. The session
	// credential takes priority over the base credential chain.
	provider := baseProvider
	if ctx.Credential != nil && !mc.Credential.IsZero() {
		p, err := BuildProvider(mc, b.Cfg.Provider, ctx.Credential)
		if err == nil {
			provider = p
			fmt.Fprintf(os.Stderr, "[auth] builder: using per-session credential for model %q\n", mc.Name)
		} else {
			fmt.Fprintf(os.Stderr, "[auth] builder: failed to build per-session provider: %v\n", err)
		}
	} else if ctx.Model != "" || mc.Name != b.mc.Name {
		// Model changed but no session credential — rebuild provider with
		// the alternative model config.
		p, err := BuildProvider(mc, b.Cfg.Provider, nil)
		if err == nil {
			provider = p
			fmt.Fprintf(os.Stderr, "[auth] builder: using per-turn model %q (no session credential)\n", mc.Name)
		}
	} else {
		fmt.Fprintf(os.Stderr, "[auth] builder: using base provider (ctx.Credential=%v, mc.Credential.IsZero=%v)\n",
			ctx.Credential != nil, mc.Credential.IsZero())
	}

	// Resolve skills for the session's workspace.
	workspacePath := ctx.Session.WorkspacePath
	var skillReg *skills.Registry
	if inst, err := b.WSReg.Get(workspacePath); err == nil {
		skillReg = inst.SkillReg
	}

	runner := BuildRunner(b.Cfg, mc, provider, b.ToolReg, skillReg, ctx.Approver, ctx.Publisher, b.rules)
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
	runner.Checkpointer = ctx.Checkpointer // mid-turn crash-safety (v1.2 §2); nil in headless builds
	runner.Stream = true                   // emit token_delta events for live "thinking" feel on the client
	return runner
}
