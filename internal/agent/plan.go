package agent

import "time"

// RunnerRef holds a *Runner for late binding. Plan tools are constructed before
// the Runner exists (tools → registry → runner), so they take a *RunnerRef whose
// R field is set after buildRunner returns.
type RunnerRef struct {
	R *Runner
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

// Plan is an implementation plan produced during plan mode. It is written to disk
// as a .md file under .codeagent/plans/ so the user can review it outside the tool.
type Plan struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Content   string     `json:"content"`   // markdown body
	Steps     []PlanStep `json:"steps,omitempty"`
	Status    PlanStatus `json:"status"`
	FilePath  string     `json:"file_path,omitempty"` // absolute path on disk
	CreatedAt time.Time  `json:"created_at"`
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
