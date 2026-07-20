package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/approve"
	"code-agent/internal/conversation"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/skills"
	"code-agent/internal/tools"
	"code-agent/internal/tools/skill"
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
	// rules is the process-wide permission store, created once so a grant persists
	// across turns. Interactive "Always allow" over the wire is not wired yet, but
	// rules loaded from config + settings files are enforced here.
	rules *approve.RuleStore

	mu         sync.RWMutex
	mc         app.ModelConfig
	provider   model.Provider
	credential credential.Resolver
}

// NewServeRunBuilder constructs the builder with the initial model + provider.
func NewServeRunBuilder(cfg app.Config, mc app.ModelConfig, provider model.Provider, cred credential.Resolver, toolReg *tools.Registry, wsReg *WorkspaceRegistry, _ *agent.RunnerRef) *ServeRunBuilder {
	return &ServeRunBuilder{
		Cfg: cfg, ToolReg: toolReg, WSReg: wsReg,
		// Pre-GA: RuleStore should be workspace-scoped via WorkspaceInstance.Rules
		// (Phase 3). For now, seed from config permissions only — workspace-level
		// persistence (scopeProjectLocal) resolves against the conversation's
		// workspacePath rather than a fixed daemon root via the build-time routing
		// below. The empty root means "no project-local persistence path," so
		// grants default to the user scope, which is workspace-independent and
		// correct for serve mode until Phase 3 lands.
		rules: approve.NewRuleStore("", cfg.Permissions.Allow, cfg.Permissions.Deny),
		mc:    mc, provider: provider, credential: cred,
	}
}

// Rules exposes the process-wide permission store so the server layer can share
// it with the RemoteApprover (which grants a client's "always allow" into it) —
// the same instance the per-turn allowlist reads, so a grant takes effect at once.
func (b *ServeRunBuilder) Rules() *approve.RuleStore { return b.rules }

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
	defer b.mu.RUnlock()
	if wireModel == "" {
		return b.mc.Model, nil
	}
	if selected, err := b.Cfg.SelectModel(wireModel); err == nil {
		return selected.Model, nil
	}
	// Gateway accepts a direct wire model name that is not a local profile.
	return wireModel, nil
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
	mc, baseProvider, baseCredential := b.mc, b.provider, b.credential
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
	if ctx.ResolvedModel != "" {
		mc.Model = ctx.ResolvedModel
	}

	// If the session has a per-session credential (server mode — JWT from
	// Authorization header), build a provider that uses it. The session
	// credential takes priority over the base credential chain.
	provider := baseProvider
	if ctx.Credential != nil && !mc.Credential.IsZero() {
		p, err := BuildProvider(mc, b.Cfg.Provider, ctx.Credential)
		if err == nil {
			provider = p
		} else {
			fmt.Fprintf(os.Stderr, "[auth] builder: failed to build per-session provider: %v\n", err)
		}
	} else if ctx.Model != "" || mc.Name != b.mc.Name {
		// Model changed but no session credential — rebuild provider with
		// the alternative model config.
		p, err := BuildProvider(mc, b.Cfg.Provider, nil)
		if err == nil {
			provider = p
		}
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
	toolCredential := baseCredential
	if ctx.Credential != nil {
		toolCredential = ctx.Credential
	}
	turnTools.Replace(websearch.NewTool(cfg.Web, toolCredential))
	planRef := &agent.RunnerRef{}
	turnTools.Replace(agent.NewEnterPlanModeTool(planRef))
	turnTools.Replace(agent.NewProposePlanTool(planRef, filepath.Join(workspacePath, ".codeagent", "plans")))

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

	runner := BuildRunner(b.Cfg, mc, provider, turnTools, skillReg, ctx.Approver, ctx.Publisher, b.rules, workspacePath)
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
	// Wire only this turn's plan tools, approver, and client tool waiter. No
	// runner reference escapes the per-turn registry.
	planRef.R = runner
	runner.PlanApprover = ctx.PlanApprover
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
