package runtime

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

// SubAgentMaxSteps bounds one delegated investigation, counted in loop iterations
// (each of which may batch several tool calls). Broad, multi-file investigation is
// the subagent's primary job, so this is generous enough to converge on a wide
// trace while still capping cost — 12 proved too tight for "map everything" tasks.
const SubAgentMaxSteps = 20

// ReadOnlyToolNames is the subagent's allow-list: the built-in tools that only
// read. It is an explicit allow-list (default-deny via tools.Subset) — a tool
// reaches the subagent ONLY by being named here, so a new write tool or an
// external MCP tool never leaks into the unattended, approval-less subagent.
// `task` is deliberately absent, which caps recursion at depth 1.
var ReadOnlyToolNames = []string{
	"read_file", "list_files", "grep", "project_graph", "git_diff", "load_skill",
}

// PlanModeToolNames is the toolset a plan-mode turn advertises: the read-only set
// plus todo_write, create_file (restricted to .codeagent/plans/), and propose_plan.
// Like ReadOnlyToolNames it is an allow-list, so a write tool can never leak into a
// planning turn. enter_plan_mode is also callable during plan mode (no-op).
var PlanModeToolNames = append([]string{
	"todo_write", "create_file", "propose_plan", "enter_plan_mode",
}, ReadOnlyToolNames...)

// DenyAllApprover is a fail-closed Approver: it refuses every side-effecting call. The
// subagent's toolset is already read-only, so this should be unreachable — it is
// the second wall, in case a side-effecting tool ever slips into the subset.
// Explicit (not nil) so the intent survives a refactor that might make nil mean
// "allow".
type DenyAllApprover struct{}

func (DenyAllApprover) Approve(string, json.RawMessage) bool { return false }

// SubAgent is the concrete task.SubAgent: each Run builds a fresh, isolated,
// ephemeral session and a read-only sub-runner, executes one turn, and returns
// only the final conclusion. It holds no per-call state, so one instance is shared
// across all `task` calls.
type SubAgent struct {
	Root        string
	Provider    model.Provider
	MC          app.ModelConfig
	Cfg         app.Config
	ReadOnly    *tools.Registry
	SkillsIndex string
	Store       session.Store // observability: persists the sub-session transcript
	Progress    agent.Emitter // observability: live, condensed heartbeat (nil = none)

	// Forwarder pushes the task_started/task_finished brackets into the CALLING
	// conversation's stream (P8.7 §8.4-2) — persisted under the parent's
	// partition and fanned to its live subscribers — so a client's entry card
	// can discover the delegation and open the child-stream viewer. Nil-safe
	// (nil = child-partition-only observability, the pre-§8.4-2 behavior).
	Forwarder *JobEventSink
}

// NewSubAgent builds the subagent backing the `task` tool. It picks the subagent
// model (agent.subagent_model, falling back to the parent) and freezes the
// read-only tool subset from the parent registry as it stands now — so tools added
// to the parent later (task itself, MCP tools) are never in the subagent's set.
func NewSubAgent(cfg app.Config, mc app.ModelConfig, parent model.Provider, root string, full *tools.Registry, skillsIndex string, store session.Store, progress agent.Emitter, forwarder *JobEventSink) *SubAgent {
	provider, subMC := ResolveSubAgentModel(cfg, mc, parent)
	return &SubAgent{
		Root:        root,
		Provider:    provider,
		MC:          subMC,
		Cfg:         cfg,
		ReadOnly:    tools.Subset(full, ReadOnlyToolNames...),
		SkillsIndex: skillsIndex,
		Store:       store,
		Progress:    progress,
		Forwarder:   forwarder,
	}
}

// Run executes one isolated, read-only turn and returns only its conclusion.
// The subagent's INNER tool events enter no parent timeline — the parent shows
// the `task` call via its own tool events, and keeping the noise out is the
// whole point of delegation. Only the task_started/task_finished brackets cross
// over (via Forwarder), carrying the child session id so a client can attach to
// the sub-stream. ec identifies the calling turn (owner) and the workspace.
func (s *SubAgent) Run(ctx context.Context, ec tools.ExecutionContext, taskPrompt string) (string, error) {
	workspaceRoot := ec.WorkspaceRoot
	sess, err := session.NewBuilder(workspaceRoot).
		WithBudget(s.MC.ContextWindow, s.Cfg.CompactThreshold(s.MC)).
		WithSystemPrompt(prompt.SubAgentSystemPrompt).
		WithSkillsIndex(s.SkillsIndex).
		Build()
	if err != nil {
		return "", err
	}
	sess.Model = s.MC.Model

	// Observability, two sinks fanned out by MultiEmitter:
	//   - store: persists the FULL transcript under the sub-session's id (inspect
	//     later with `codeagent task-trace <id>`), and indexes the delegation.
	//   - progress: a CONDENSED live heartbeat (run/repl), so `task` is not a black
	//     box while it runs.
	// Crucially, NEITHER is the parent's live renderer, so the full sub-stream
	// never floods the parent — default-quiet holds. task_started/finished bracket
	// the run. Both sinks nil (e.g. tests / piped output) degrades to fully quiet.
	sinks := make(MultiEmitter, 0, 2)
	if s.Store != nil {
		sinks = append(sinks, EventStoreEmitter{Ctx: ctx, Store: s.Store})
	}
	if s.Progress != nil {
		sinks = append(sinks, s.Progress)
	}
	var emitter agent.Emitter
	started := agent.Event{
		Kind: agent.EventTaskStarted, SessionID: sess.ID, TurnID: ec.TurnID,
		CallID: ec.CallID, At: time.Now(), Text: taskPrompt,
	}
	if len(sinks) > 0 {
		emitter = sinks
		emitter.Emit(started)
	}
	// Parent-stream copy (§8.4-2): the entry card discovers the delegation from
	// the calling conversation's own stream — live and on replay. Nil-safe.
	s.Forwarder.ForwardBracket(started, ec.SessionID)

	sub := &agent.Runner{
		Model:         s.Provider,
		ModelName:     s.MC.Model,
		Temperature:   s.MC.Temperature,
		Tools:         s.ReadOnly,
		MaxSteps:      SubAgentMaxSteps,
		Approver:      DenyAllApprover{}, // fail-closed; should be unreachable (read-only set)
		Observer:      observation.DefaultObserver{},
		Reflector:     agent.DefaultReflector{},
		Compactor:     BuildCompactor(s.MC, s.Provider),
		Emitter:       emitter, // store-only (or nil) — never the parent's live renderer
		WorkspaceRoot: workspaceRoot,
	}

	res, err := sub.RunTurn(ctx, sess, taskPrompt)
	if err != nil {
		return "", err
	}

	conclusion := res.Final
	if res.HitStepLimit {
		// The loop's finalAnswerAfterLimit already sanitizes a leaked tool-call
		// answer to a clean message (agent.LooksLikeToolCallLeak), so res.Final is
		// never garbage here — just mark it as a non-convergent partial result so
		// the parent can narrow the task and retry (PRD §5.4).
		conclusion = fmt.Sprintf("[subagent did not converge within %d steps — partial findings only]\n\n%s",
			SubAgentMaxSteps, res.Final)
	}
	finished := agent.Event{
		Kind: agent.EventTaskFinished, SessionID: sess.ID, TurnID: ec.TurnID,
		CallID: ec.CallID, At: time.Now(), Text: conclusion,
	}
	if emitter != nil {
		emitter.Emit(finished)
	}
	s.Forwarder.ForwardBracket(finished, ec.SessionID)
	return conclusion, nil
}

// ResolveSubAgentModel returns the provider + config the subagent should use.
// agent.subagent_model names a configured model; unset, self-referential, or
// unusable falls back to the parent's provider and model (logged) so a subagent
// always runs. NOTE: a distinct subagent model gets a FRESH provider that is not
// wired to the request-telemetry Observer, so its token usage is not yet recorded
// in the cost report — acceptable until 8.1 lands; inheriting the parent (the
// default) is fully recorded.
func ResolveSubAgentModel(cfg app.Config, mc app.ModelConfig, parent model.Provider) (model.Provider, app.ModelConfig) {
	name := cfg.Agent.SubagentModel
	if name == "" || name == mc.Name {
		return parent, mc
	}
	subMC, err := cfg.SelectModel(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[subagent] model %q unusable (%v); using %s\n", name, err, mc.Name)
		return parent, mc
	}
	subProvider, err := BuildProvider(subMC, cfg.Provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[subagent] cannot build provider for %q (%v); using %s\n", name, err, mc.Name)
		return parent, mc
	}
	return subProvider, subMC
}
