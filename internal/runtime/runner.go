package runtime

import (
	"context"
	"net/url"
	"os"
	"strings"
	"time"

	"code-agent/internal/agent"
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
func BuildCompactor(cfg settings.Settings, mc settings.ModelConfig, provider model.Provider) session.Compactor {
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
func BuildRunner(cfg settings.Settings, mc settings.ModelConfig, provider model.Provider, registry *tools.Registry, skillReg *skills.Registry, approver agent.Approver, emitter agent.Emitter, rules *approve.RuleStore, root string) *agent.Runner {
	// Hook runner: settings-layer hooks (the config layer no longer carries
	// hooks). Hooks run `sh -c`, so on a no-subprocess host (iOS) they are
	// suppressed — never just set.Hooks (P11.c).
	var hook agent.ToolHook
	var hookRunner *hooks.Runner
	if cfg.Profile.AllowsSubprocess() {
		allHooks := cfg.Hooks
		if hr := hooks.New(allHooks, root); hr != nil {
			hook = hr
			hookRunner = hr
		}
	}

	// Pre-approve/deny tool calls matching the shared permission RuleStore
	// (Claude-style allow/deny globs, plus any "Always allow" grant made this
	// session), outermost in the approver chain so a matched rule short-circuits
	// before auto mode or the human prompt. The store is created once per
	// process/session by the caller and shared with the frontend approver (which
	// Grants into it), so a nil store just leaves the approver unchanged.
	approver = approve.Allowlisted(rules, approver)

	runner := &agent.Runner{
		Model:            provider,
		ModelName:        mc.Model,
		Temperature:      mc.Temperature,
		Tools:            registry,
		MaxSteps:         cfg.Agent.MaxSteps,
		MaxParallelTools: cfg.Agent.MaxParallelTools,
		Approver:         approver,
		Observer:         observation.DefaultObserver{},
		RemindHypothesis: true,
		// Reflector and VerifyCommand are no longer set by default. To enable
		// deterministic build verification at the stop boundary, construct a
		// VerifyGate and assign it to runner.StopPolicy.
		PlanTools:     tools.Subset(registry, PlanModeToolNames...),
		Hook:          hook,
		HookRunner:    hookRunner,
		Compactor:     BuildCompactor(cfg, mc, provider),
		ContextEditor: &session.ContextEditor{KeepTurns: 3},
		// Tier-0 pruning shares the compactor's verbatim-tail budget (P12.c).
		CompactKeepTokens: cfg.CompactKeepTokens(mc),
		Emitter:           emitter,
		WorkspaceRoot:     root,
		SessionIndex:      SessionIndex(),
		// Fast git-status cache avoids redundant git(1) invocations per model call.
		GitCache: session.NewFastGitProvider(root, 30*time.Second),
		// Client-tool lease (0 = loop's built-in 2-minute default). Raised by
		// deployments whose client tools run long (e.g. DreamAI media generation).
		ClientToolTimeout: time.Duration(cfg.Agent.ClientToolTimeoutSeconds) * time.Second,
	}
	// after_turn stop policy (8.5): an operator-configured after_turn hook owns
	// the finalize stop decision, REPLACING the harness's built-in default
	// policy. Same sh -c runner as the tool hooks; suppressed on no-subprocess
	// hosts (P11.c), where StopPolicy stays nil and the default applies.
	if hookRunner != nil && hookRunner.HasAfterTurn() {
		stopHook := hookRunner
		runner.StopPolicy = agent.StopPolicyFunc(func(ctx context.Context, sc agent.StopContext) (agent.StopVerdict, error) {
			v := stopHook.StopDecide(ctx, stopHookInput(sc))
			return agent.StopVerdict{Continue: v.Continue, Message: v.Message}, nil
		})
	}
	// Asset upload is a Gateway-only capability. Direct OpenAI-compatible and
	// Ollama models must never receive Gateway asset references by accident.
	if isGatewayModelEndpoint(mc.BaseURL) {
		runner.UserAssetsSupported = true
		if uploader, ok := provider.(model.AssetUploader); ok {
			runner.AssetUploader = uploader
		}
	}
	// Vision input is a per-model capability, but the injection only fires when
	// the wire transport can actually serialize content parts. OpenAI-compatible
	// and Responses both map ContentParts to their native image blocks
	// (image_url / input_image); an unsupported transport (ollama today) keeps
	// the textual manifest fallback instead of silently dropping images. The
	// loop decides per request.
	runner.VisionSupported = mc.SupportsVision() &&
		(mc.Provider == "" || mc.Provider == "openai" || mc.Provider == "responses")
	return runner
}

// stopHookInput converts the loop's StopContext snapshot into the JSON shape an
// after_turn hook receives on stdin. Extracted so the field mapping is unit
// testable without standing up a full BuildRunner.
func stopHookInput(sc agent.StopContext) hooks.StopHookInput {
	in := hooks.StopHookInput{
		LastText:    sc.LastText,
		PlanState:   sc.PlanState.String(),
		CodeMutated: sc.CodeMutated,
		ToolCalls:   sc.ToolCalls,
		MaxSteps:    sc.MaxSteps,
	}
	for _, td := range sc.Todos {
		in.Todos = append(in.Todos, hooks.StopHookTodo{Content: td.Content, Status: string(td.Status)})
	}
	return in
}

func isGatewayModelEndpoint(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/api/v1/agent")
}
