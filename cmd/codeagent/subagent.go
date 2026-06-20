package main

import (
	"code-agent/internal/agent"
	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/observation"
	"code-agent/internal/prompt"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// subAgentMaxSteps bounds one delegated investigation. Small on purpose: a
// read-only subagent should converge quickly, and this caps its cost and latency.
const subAgentMaxSteps = 12

// readOnlyToolNames is the subagent's allow-list: the built-in tools that only
// read. It is an explicit allow-list (default-deny via tools.Subset) — a tool
// reaches the subagent ONLY by being named here, so a new write tool or an
// external MCP tool never leaks into the unattended, approval-less subagent.
// `task` is deliberately absent, which caps recursion at depth 1.
var readOnlyToolNames = []string{
	"read_file", "list_files", "grep", "project_graph", "git_diff", "load_skill",
}

// denyAll is a fail-closed Approver: it refuses every side-effecting call. The
// subagent's toolset is already read-only, so this should be unreachable — it is
// the second wall, in case a side-effecting tool ever slips into the subset.
// Explicit (not nil) so the intent survives a refactor that might make nil mean
// "allow".
type denyAll struct{}

func (denyAll) Approve(string, json.RawMessage) bool { return false }

// subAgent is the concrete task.SubAgent: each Run builds a fresh, isolated,
// ephemeral session and a read-only sub-runner, executes one turn, and returns
// only the final conclusion. It holds no per-call state, so one instance is shared
// across all `task` calls.
type subAgent struct {
	root        string
	provider    model.Provider
	mc          app.ModelConfig
	cfg         app.Config
	readOnly    *tools.Registry
	skillsIndex string
	store       session.Store // for observability: persists the sub-session transcript
}

// newSubAgent builds the subagent backing the `task` tool. It picks the subagent
// model (agent.subagent_model, falling back to the parent) and freezes the
// read-only tool subset from the parent registry as it stands now — so tools added
// to the parent later (task itself, MCP tools) are never in the subagent's set.
func newSubAgent(cfg app.Config, mc app.ModelConfig, parent model.Provider, root string, full *tools.Registry, skillsIndex string, store session.Store) *subAgent {
	provider, subMC := resolveSubAgentModel(cfg, mc, parent)
	return &subAgent{
		root:        root,
		provider:    provider,
		mc:          subMC,
		cfg:         cfg,
		readOnly:    tools.Subset(full, readOnlyToolNames...),
		skillsIndex: skillsIndex,
		store:       store,
	}
}

// Run executes one isolated, read-only turn and returns only its conclusion. The
// sub-runner has NO Emitter (default quiet): the subagent's own tool events enter
// no timeline — the parent already shows the `task` call via its own tool events,
// and keeping the noise out is the whole point of delegation.
func (s *subAgent) Run(ctx context.Context, taskPrompt string) (string, error) {
	sess, err := session.NewBuilder(s.root).
		WithBudget(s.mc.ContextWindow, s.cfg.CompactThreshold(s.mc)).
		WithSystemPrompt(prompt.SubAgentSystemPrompt).
		WithSkillsIndex(s.skillsIndex).
		Build()
	if err != nil {
		return "", err
	}
	sess.Model = s.mc.Model

	// Observability: the sub-runner emits to a STORE-ONLY emitter, so its full
	// investigation is persisted under the sub-session's id — inspect it with
	// `codeagent task-trace <id>` — WITHOUT rendering into the parent's live view,
	// so default-quiet is preserved. task_started/finished bracket the transcript
	// and index the delegation for `codeagent tasks`. A nil store (e.g. in tests)
	// degrades to fully quiet.
	var emitter agent.Emitter
	if s.store != nil {
		emitter = eventStoreEmitter{ctx: ctx, store: s.store}
		emitter.Emit(agent.Event{Kind: agent.EventTaskStarted, SessionID: sess.ID, At: time.Now(), Text: taskPrompt})
	}

	sub := &agent.Runner{
		Model:       s.provider,
		ModelName:   s.mc.Model,
		Temperature: s.mc.Temperature,
		Tools:       s.readOnly,
		MaxSteps:    subAgentMaxSteps,
		Approver:    denyAll{}, // fail-closed; should be unreachable (read-only set)
		Observer:    observation.DefaultObserver{},
		Reflector:   agent.DefaultReflector{},
		Compactor:   buildCompactor(s.mc, s.provider),
		Emitter:     emitter, // store-only (or nil) — never the parent's live renderer
	}

	res, err := sub.RunTurn(ctx, sess, taskPrompt)
	if err != nil {
		return "", err
	}

	conclusion := res.Final
	if res.HitStepLimit {
		// Don't pass off a non-convergent run as a clean answer; mark it so the
		// parent can decide to narrow the task and retry (PRD §5.4).
		conclusion = fmt.Sprintf("[subagent did not converge within %d steps — partial findings only]\n\n%s",
			subAgentMaxSteps, res.Final)
	}
	if emitter != nil {
		emitter.Emit(agent.Event{Kind: agent.EventTaskFinished, SessionID: sess.ID, At: time.Now(), Text: conclusion})
	}
	return conclusion, nil
}

// resolveSubAgentModel returns the provider + config the subagent should use.
// agent.subagent_model names a configured model; unset, self-referential, or
// unusable falls back to the parent's provider and model (logged) so a subagent
// always runs. NOTE: a distinct subagent model gets a FRESH provider that is not
// wired to the request-telemetry Observer, so its token usage is not yet recorded
// in the cost report — acceptable until 8.1 lands; inheriting the parent (the
// default) is fully recorded.
func resolveSubAgentModel(cfg app.Config, mc app.ModelConfig, parent model.Provider) (model.Provider, app.ModelConfig) {
	name := cfg.Agent.SubagentModel
	if name == "" || name == mc.Name {
		return parent, mc
	}
	subMC, err := cfg.SelectModel(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[subagent] model %q unusable (%v); using %s\n", name, err, mc.Name)
		return parent, mc
	}
	subProvider, err := buildProvider(subMC, cfg.Provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[subagent] cannot build provider for %q (%v); using %s\n", name, err, mc.Name)
		return parent, mc
	}
	return subProvider, subMC
}
