package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/approve"
	"code-agent/internal/conversation"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
	"code-agent/internal/tools/skill"
	"code-agent/internal/tools/task"
	"code-agent/internal/tools/websearch"
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
	Cfg app.Config
	// ToolReg is the shared BASE registry (built-ins, no MCP). Build prefers the
	// session workspace's own registry (base + that workspace's MCP tools) and
	// falls back to this when the workspace has no MCP config or fails to resolve.
	ToolReg *tools.Registry
	WSReg   *WorkspaceRegistry
	// rules is the process-wide fallback permission store (no project root).
	// Per-workspace RuleStores are in wsRules and take priority when the
	// session has a known workspacePath.
	rules *approve.RuleStore

	mu         sync.RWMutex
	mc         app.ModelConfig
	provider   model.Provider
	credential credential.Resolver

	wsRulesMu sync.Mutex
	wsRules   map[string]*approve.RuleStore // workspacePath → scoped store
}

// NewServeRunBuilder constructs the builder with the initial model + provider.
func NewServeRunBuilder(cfg app.Config, mc app.ModelConfig, provider model.Provider, cred credential.Resolver, toolReg *tools.Registry, wsReg *WorkspaceRegistry, _ *agent.RunnerRef) *ServeRunBuilder {
	return &ServeRunBuilder{
		Cfg: cfg, ToolReg: toolReg, WSReg: wsReg,
		rules:   approve.NewRuleStore("", cfg.Permissions.Allow, cfg.Permissions.Deny),
		wsRules: make(map[string]*approve.RuleStore),
		mc:      mc, provider: provider, credential: cred,
	}
}

// Rules exposes the process-wide permission store so the server layer can share
// it with the RemoteApprover (which grants a client's "always allow" into it) —
// the same instance the per-turn allowlist reads, so a grant takes effect at once.
func (b *ServeRunBuilder) Rules() *approve.RuleStore { return b.rules }

// workspaceRules returns the permission store for a workspace, creating one
// lazily if needed. Each workspace gets its own RuleStore so grants are scoped
// to that workspace's .codeagent/settings.local.json rather than the user-global
// settings file — preventing workspace pollution. When root is empty (server
// default), falls back to the process-wide store.
func (b *ServeRunBuilder) workspaceRules(root string) *approve.RuleStore {
	if root == "" {
		return b.rules
	}
	b.wsRulesMu.Lock()
	ws, ok := b.wsRules[root]
	if !ok {
		ws = approve.NewRuleStore(root, b.Cfg.Permissions.Allow, b.Cfg.Permissions.Deny)
		b.wsRules[root] = ws
	}
	b.wsRulesMu.Unlock()
	return ws
}

// Reconfigure hot-swaps the model config and provider used by future turns
// (v1.2 §3.3). It does not touch the listener or any in-flight turn.
func (b *ServeRunBuilder) Reconfigure(mc app.ModelConfig, provider model.Provider, cred credential.Resolver) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mc = mc
	b.provider = provider
	b.credential = cred
}

func (b *ServeRunBuilder) ResolveModel(wireModel string) (string, error) {
	b.mu.RLock()
	defaultMC, cfg := b.mc, b.Cfg
	b.mu.RUnlock()
	selected, err := resolveTurnModel(cfg, defaultMC, wireModel)
	if err != nil {
		return "", err
	}
	return selected.Model, nil
}

// ModelNotConfiguredError is returned before turn acceptance when an Embedded
// Runtime was intentionally started with models: {}.
type ModelNotConfiguredError struct{}

func (ModelNotConfiguredError) Error() string { return "no model provider is configured" }
func (ModelNotConfiguredError) AgentInputErrorCode() string {
	return "model_not_configured"
}
func (ModelNotConfiguredError) LifecycleErrorCode() string {
	return "model_not_configured"
}
func (ModelNotConfiguredError) SafeMessage() string {
	return "Connect a model provider before sending a message."
}

// ModelUnavailableError is returned before turn acceptance when a requested
// Runtime Alias is not present in the configuration currently applied to the
// Runtime. This can happen briefly while a host is publishing a new Provider
// connection; it must be visible to clients instead of leaving the submission
// waiting forever in the dispatched state.
type ModelUnavailableError struct {
	Requested string
	Cause     error
}

func (e ModelUnavailableError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("model %q is unavailable", e.Requested)
	}
	return fmt.Sprintf("model %q is unavailable: %v", e.Requested, e.Cause)
}

func (e ModelUnavailableError) Unwrap() error { return e.Cause }
func (ModelUnavailableError) AgentInputErrorCode() string {
	return "model_unavailable"
}
func (ModelUnavailableError) LifecycleErrorCode() string {
	return "model_unavailable"
}
func (ModelUnavailableError) SafeMessage() string {
	return "The selected model is not available. Wait for provider configuration to finish applying, then try again."
}

// resolveTurnModel maps the client model field to a complete ModelConfig. The
// provider.* namespace is reserved for AgentKit Runtime Aliases and is always
// strict. A legacy bare wire-model string is accepted only while the runtime's
// default endpoint is the Talkify Gateway.
func resolveTurnModel(cfg app.Config, defaultMC app.ModelConfig, requested string) (app.ModelConfig, error) {
	if cfg.Models != nil && len(cfg.Models) == 0 {
		return app.ModelConfig{}, ModelNotConfiguredError{}
	}
	if requested == "" {
		return defaultMC, nil
	}
	if selected, err := cfg.SelectModel(requested); err == nil {
		return selected, nil
	} else if strings.HasPrefix(requested, "provider.") {
		return app.ModelConfig{}, ModelUnavailableError{Requested: requested, Cause: err}
	}
	if !isGatewayModelEndpoint(defaultMC.BaseURL) {
		return app.ModelConfig{}, ModelUnavailableError{
			Requested: requested,
			Cause:     errors.New("direct providers require a configured Runtime Alias"),
		}
	}
	legacy := defaultMC
	legacy.Model = requested
	return legacy, nil
}

// effectiveCredentialResolver gives a connection-scoped session credential
// priority without hiding AgentKit-injected Direct Provider credentials.
func effectiveCredentialResolver(session, base credential.Resolver) credential.Resolver {
	switch {
	case session == nil:
		return base
	case base == nil:
		return session
	default:
		return &credential.ChainResolver{
			Resolvers: []credential.Resolver{session, base},
		}
	}
}

// ImageInputCapability probes the frozen Runtime–Gateway contract using the
// credential bound to the connecting conversation. The provider owns the
// 60-second base-URL + credential-scope cache and fails closed when stale.
func (b *ServeRunBuilder) ImageInputCapability(ctx context.Context, cred credential.Resolver) bool {
	provider, ok := b.gatewayProvider(cred)
	if !ok {
		return false
	}
	prober, ok := provider.(model.ImageInputCapabilityProber)
	if !ok {
		return false
	}
	enabled, err := prober.ImageInputCapability(ctx)
	return err == nil && enabled
}

func (b *ServeRunBuilder) gatewayProvider(cred credential.Resolver) (model.Provider, bool) {
	b.mu.RLock()
	mc, pc, baseCred := b.mc, b.Cfg.Provider, b.credential
	b.mu.RUnlock()
	if !isGatewayModelEndpoint(mc.BaseURL) {
		return nil, false
	}
	if cred == nil {
		cred = baseCred
	}
	provider, err := BuildProvider(mc, pc, cred)
	return provider, err == nil
}

func (b *ServeRunBuilder) CredentialScope(ctx context.Context, cred credential.Resolver) string {
	provider, ok := b.gatewayProvider(cred)
	if !ok {
		return ""
	}
	scoper, ok := provider.(model.AssetUploadScoper)
	if !ok {
		return ""
	}
	return scoper.AssetUploadScope(ctx)
}

func (b *ServeRunBuilder) ReleaseConversationAssetRefs(ctx context.Context, cred credential.Resolver, sessionID string) error {
	provider, ok := b.gatewayProvider(cred)
	if !ok {
		return errors.New("gateway provider unavailable")
	}
	releaser, ok := provider.(model.ConversationAssetRefReleaser)
	if !ok {
		return errors.New("gateway asset-ref release unavailable")
	}
	return releaser.ReleaseConversationAssetRefs(ctx, sessionID)
}

// Build creates a per-turn TurnRunner that resolves skills from the session's
// workspace, merges client-registered tools, and wires plan tools + client waiter.
func (b *ServeRunBuilder) Build(ctx conversation.RuntimeContext) conversation.TurnRunner {
	b.mu.RLock()
	defaultMC, baseProvider, baseCredential := b.mc, b.provider, b.credential
	cfg := b.Cfg
	b.mu.RUnlock()

	mc, modelErr := resolveTurnModel(cfg, defaultMC, ctx.Model)
	if modelErr != nil {
		return failedTurnRunner{err: modelErr}
	}
	if ctx.ResolvedModel != "" {
		mc.Model = ctx.ResolvedModel
	}

	// Rebuild whenever the turn selected a model or supplied a session
	// credential. The effective chain is session → injected/configured/env.
	effectiveCredential := effectiveCredentialResolver(ctx.Credential, baseCredential)
	provider := baseProvider
	if ctx.Model != "" || ctx.Credential != nil || mc.Name != defaultMC.Name {
		p, err := BuildProvider(mc, cfg.Provider, effectiveCredential)
		if err != nil {
			return failedTurnRunner{err: fmt.Errorf("build provider for model %q: %w", mc.Name, err)}
		}
		inheritProviderObserver(baseProvider, p)
		provider = p
	}

	// Resolve skills AND tools for the session's workspace. The workspace instance
	// carries its own tool registry (base built-ins + the workspace's MCP tools,
	// from <workspace>/.mcp.json); a workspace without MCP has a nil ToolReg and
	// uses the shared base registry directly. This is what makes the MCP tool set
	// follow conversation.workspace_path instead of the daemon's launch directory.
	workspacePath := ctx.Session.WorkspacePath
	var skillReg *skills.Registry
	toolReg := b.ToolReg
	if inst, err := b.WSReg.Get(workspacePath); err == nil {
		skillReg = inst.SkillReg
		// Hot-reload .mcp.json changes before every turn (Phase 2b).
		if inst.CheckReloadMCP() {
			inst.ReloadMCP()
		}
		if inst.ToolReg != nil {
			toolReg = inst.ToolReg
		}
	}

	// The base registry's plan tools carry a late-bound RunnerRef. They must be
	// replaced in every turn-local clone: wiring the shared base reference here
	// races concurrent sessions and can route A's plan operation into B's runner.
	turnTools := toolReg.Clone()
	if searchTool := websearch.NewTool(cfg.Web, effectiveCredential); searchTool != nil {
		turnTools.Replace(searchTool)
	}
	planRef := &agent.RunnerRef{}
	turnTools.Replace(agent.NewEnterPlanModeTool(planRef))
	turnTools.Replace(agent.NewProposePlanTool(planRef, filepath.Join(workspacePath, ".codeagent", "plans")))
	turnTools.Replace(agent.NewAskUserTool(planRef))

	// Replace the shared LoadSkillTool with one that uses this workspace's own
	// skill registry, so hot-reload (triggered on cache miss) only affects this
	// workspace — never leaking skills across workspaces (§6).
	if skillReg != nil {
		turnTools.Replace(skill.NewLoadSkillTool(
			skillReg,
			cfg.GlobalSkillsDir,
			filepath.Join(workspacePath, "skills"),
		))
	}
	// task is also provider-backed. Rebind it after the workspace load_skill
	// replacement so the subagent inherits this turn's model, resolver, and
	// workspace-scoped read-only tools.
	rebindTurnTask(turnTools, cfg, mc, provider, effectiveCredential, workspacePath, skillReg)

	// Resolve the permission store for this workspace. Each workspace gets its
	// own RuleStore so an "Always allow" grant stays scoped to that workspace's
	// .codeagent/settings.local.json and never pollutes other workspaces.
	wsRules := b.workspaceRules(workspacePath)
	if ra, ok := ctx.Approver.(interface{ SetGranter(approve.Granter) }); ok {
		ra.SetGranter(wsRules)
	}

	runner := BuildRunner(b.Cfg, mc, provider, turnTools, skillReg, ctx.Approver, ctx.Publisher, wsRules, workspacePath)
	runner.ReservedTurnID = ctx.TurnID
	runner.RequestID = ctx.RequestID
	if workspacePath != "" {
		runner.WorkspaceRoot = workspacePath
	}
	// Merge client-registered tools into a per-turn clone so the shared registry stays unmodified.
	if len(ctx.ClientTools) > 0 {
		reg := turnTools
		for _, def := range ctx.ClientTools {
			proxy := tools.NewClientProxyTool(def.Name, def.Description, def.InputSchema)
			if err := reg.Register(proxy); err != nil {
				continue // name collision with a server tool — skip
			}
		}
		runner.Tools = reg
	}
	// plan_workflow is turn-scoped: bind the selected provider/model after model
	// override and after client tools have been merged. Its Execute path projects
	// runner.Tools, so workspace MCP and session client tools become DAG nodes.
	runner.Tools.Replace(NewFluxWorkflowTool(provider, mc.Model))
	// Wire only this turn's plan tools, approver, and client tool waiter. No
	// runner reference escapes the per-turn registry.
	planRef.R = runner
	runner.PlanApprover = ctx.PlanApprover
	runner.AskUserApprover = ctx.AskUserApprover
	runner.ClientWaiter = ctx.ClientWaiter

	// If the approver can also gate external path access (as RemoteApprover
	// does), wire it so read tools can request user approval for paths outside
	// the workspace instead of hard-rejecting.
	if pa, ok := ctx.Approver.(tools.PathAccessApprover); ok {
		runner.PathAccessApprover = pa
	}
	runner.Checkpointer = ctx.Checkpointer // mid-turn crash-safety (v1.2 §2); nil in headless builds
	runner.Stream = true                   // emit final-text and reasoning deltas for live client rendering
	return runner
}

// failedTurnRunner preserves RunBuilder's historical no-error factory signature
// while making a strict Alias failure visible to recovery/direct Build callers.
// Normal agent_input requests fail earlier in ResolveModel, before acceptance.
type failedTurnRunner struct{ err error }

func (r failedTurnRunner) RunTurn(context.Context, *session.Session, string) (agent.TurnResult, error) {
	return agent.TurnResult{}, r.err
}

func (r failedTurnRunner) ResumeTurn(context.Context, *session.Session) (agent.TurnResult, error) {
	return agent.TurnResult{}, r.err
}

func rebindTurnTask(registry *tools.Registry, cfg app.Config, mc app.ModelConfig, provider model.Provider, cred credential.Resolver, root string, skillReg *skills.Registry) {
	raw, ok := registry.Get("task")
	if !ok {
		return
	}
	taskTool, ok := raw.(*task.Tool)
	if !ok {
		return
	}
	template, ok := taskTool.Agent().(*SubAgent)
	if !ok {
		return
	}
	skillsIndex := ""
	if skillReg != nil {
		skillsIndex = skillReg.PromptIndex()
	}
	registry.Replace(task.NewTool(
		template.Rebind(cfg, mc, provider, cred, root, registry, skillsIndex),
	))
}
