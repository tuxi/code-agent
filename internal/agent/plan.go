package agent

import "time"

// RunnerRef holds a *Runner for late binding. Plan tools are constructed before
// the Runner exists (tools → registry → runner), so they take a *RunnerRef whose
// R field is set after buildRunner returns.
type RunnerRef struct {
	R *Runner
}

// BeginPlanning starts a fresh discovery cycle. All prior critic/reviewer
// verdicts are scoped to the old plan and must not leak into the new one.
func (r *Runner) BeginPlanning(title string) {
	r.SetPlanState(PlanStatusPlanning)
	r.planTitle = title
	r.activePlan = nil
	r.lastAssistantText = ""
	r.planCriticPassed = false
	r.planCriticPath = ""
	r.planCriticDigest = ""
	r.plannedMutation = false
	r.independentReviewPassed = false
}

// SetPlanState transitions the plan state machine and emits a
// plan_state_changed event so clients can render plan-mode state without
// polling (10.1). It is the single mutation point for Runner.PlanState; every
// transition — entering plan mode, proposing, approving, rejecting, or
// exiting — funnels through here. No-op when the state is unchanged (e.g.
// enter_plan_mode's already-in-plan-mode guard), so a redundant transition
// never spams the event stream.
func (r *Runner) SetPlanState(s PlanStatus) {
	if r.PlanState == s {
		return
	}
	r.PlanState = s
	r.emit(Event{Kind: EventPlanStateChanged, PlanState: s})
}

func (r *Runner) independentTaskAvailable() bool {
	registry := r.Tools
	if r.PlanState == PlanStatusPlanning && r.PlanTools != nil {
		registry = r.PlanTools
	}
	if registry == nil {
		return false
	}
	_, ok := registry.Get("task")
	return ok
}

// PlanStatus tracks which phase of the planning workflow the runner is in.
// It replaces the old PlanMode bool with a proper state machine.
type PlanStatus int

const (
	PlanStatusNone      PlanStatus = iota // no plan active
	PlanStatusPlanning                    // model is researching, restricted tools
	PlanStatusProposing                   // plan is written, awaiting user approval
	PlanStatusApproved                    // user approved the plan
	PlanStatusRejected                    // user rejected the plan
	PlanStatusExecuting                   // approved plan is being implemented
)

// String renders the stable wire form of a plan state (plan_state_changed).
func (s PlanStatus) String() string {
	switch s {
	case PlanStatusPlanning:
		return "planning"
	case PlanStatusProposing:
		return "proposing"
	case PlanStatusApproved:
		return "approved"
	case PlanStatusRejected:
		return "rejected"
	case PlanStatusExecuting:
		return "executing"
	default:
		return "none"
	}
}

// Plan is an implementation plan produced during plan mode. It is written to disk
// as a .md file under .codeagent/plans/ so the user can review it outside the tool.
type Plan struct {
	ID      string     `json:"id"`
	Title   string     `json:"title"`
	Content string     `json:"content"` // markdown body
	Steps   []PlanStep `json:"steps,omitempty"`
	Status  PlanStatus `json:"status"`
	// Readiness records the evidence and verification contract checked before the
	// plan can cross from discovery into implementation.
	Readiness PlanReadiness `json:"readiness"`
	// WorkspaceRelativePath is the stable, portable path clients should display.
	// FilePath is the server-local absolute path clients may use when they share
	// the server's filesystem.
	WorkspaceRelativePath string    `json:"workspace_relative_path,omitempty"`
	FilePath              string    `json:"file_path,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

type PlanReadiness struct {
	EvidencePaths    []string `json:"evidence_paths"`
	Verification     []string `json:"verification"`
	BlockingUnknowns []string `json:"blocking_unknowns"`
	CriticSummary    string   `json:"critic_summary"`
}

// PlanStep is a single actionable step within a plan. Steps are populated from the
// model's todo_write calls during execution and mapped back to the plan.
type PlanStep struct {
	Description string `json:"description"`
	Status      string `json:"status"` // pending, in_progress, completed
}

// PlanDecision is the user's verdict on a proposed plan.
type PlanDecision int

const (
	PlanApproved PlanDecision = iota
	PlanRejected
)

// PlanApprover approves or rejects a proposed implementation plan. The call
// blocks until the user decides — the same synchronous blocking pattern as the
// existing agent.Approver.
type PlanApprover interface {
	ApprovePlan(plan Plan) PlanDecision
}
