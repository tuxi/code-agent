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
	store       session.Store // observability: persists the sub-session transcript
	progress    agent.Emitter // observability: live, condensed heartbeat (nil = none)
}

// newSubAgent builds the subagent backing the `task` tool. It picks the subagent
// model (agent.subagent_model, falling back to the parent) and freezes the
// read-only tool subset from the parent registry as it stands now — so tools added
// to the parent later (task itself, MCP tools) are never in the subagent's set.
func newSubAgent(cfg app.Config, mc app.ModelConfig, parent model.Provider, root string, full *tools.Registry, skillsIndex string, store session.Store, progress agent.Emitter) *subAgent {
	provider, subMC := resolveSubAgentModel(cfg, mc, parent)
	return &subAgent{
		root:        root,
		provider:    provider,
		mc:          subMC,
		cfg:         cfg,
		readOnly:    tools.Subset(full, readOnlyToolNames...),
		skillsIndex: skillsIndex,
		store:       store,
		progress:    progress,
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

	// Observability, two sinks fanned out by multiEmitter:
	//   - store: persists the FULL transcript under the sub-session's id (inspect
	//     later with `codeagent task-trace <id>`), and indexes the delegation.
	//   - progress: a CONDENSED live heartbeat (run/repl), so `task` is not a black
	//     box while it runs.
	// Crucially, NEITHER is the parent's live renderer, so the full sub-stream
	// never floods the parent — default-quiet holds. task_started/finished bracket
	// the run. Both sinks nil (e.g. tests / piped output) degrades to fully quiet.
	sinks := make(multiEmitter, 0, 2)
	if s.store != nil {
		sinks = append(sinks, eventStoreEmitter{ctx: ctx, store: s.store})
	}
	if s.progress != nil {
		sinks = append(sinks, s.progress)
	}
	var emitter agent.Emitter
	if len(sinks) > 0 {
		emitter = sinks
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
