package server

import (
	"encoding/json"

	"code-agent/internal/approve"
)

// Control-plane messages (docs/protocols/agent-wire-v1.md §4). Unlike events
// (server->client, fire-and-forget), these are request/response and flow in both
// directions over the same connection. v1 defines approval; switch_model and
// goal_start are reserved for the same envelope.

// MsgTypeApprovalResponse is the client -> server control-plane reply discriminator.
const MsgTypeApprovalResponse = "approval_response"

// ApprovalRequest is sent server->client mid-turn when a side-effecting tool
// needs a human decision. It maps directly onto the core Approver.Approve call,
// which blocks until the matching ApprovalResponse arrives (or the deadline /
// disconnect defaults to denial).
type ApprovalRequest struct {
	Type       string          `json:"type"` // always "approval_request"
	ID         string          `json:"id"`   // correlates with ApprovalResponse.ID
	SessionID  string          `json:"session_id,omitempty"`
	TurnID     string          `json:"turn_id,omitempty"`
	ToolName   string          `json:"tool_name"`
	ToolArgs   json.RawMessage `json:"tool_args,omitempty"`
	DeadlineMS int64           `json:"deadline_ms,omitempty"`
}

// ApprovalResponse is the client's verdict, correlated to a request by ID.
//
// Decision is the richer, Claude-style three-way verdict a client may send:
// "once" (allow this call), "always" (allow + persist a permission rule so future
// matching calls skip the prompt), or "deny". Scope selects where an "always"
// grant is persisted: "local" (project-local, the default) or "user". Approved is
// the legacy two-state field: a client that only sends Approved still works, and
// Decision, when present, takes precedence (see outcome).
type ApprovalResponse struct {
	Type     string `json:"type"` // always "approval_response"
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
	Decision string `json:"decision,omitempty"` // "once" | "always" | "deny"
	Scope    string `json:"scope,omitempty"`    // "local" | "user" (for "always")
}

// outcome resolves the response into (approved, always, scope). Decision wins
// when set; otherwise it falls back to the legacy Approved boolean.
func (m ApprovalResponse) outcome() (approved, always bool, scope approve.Scope) {
	scope = approve.ParseScope(m.Scope)
	switch m.Decision {
	case "always":
		return true, true, scope
	case "once":
		return true, false, scope
	case "deny":
		return false, false, scope
	default:
		return m.Approved, false, scope
	}
}

// PlanApprovalRequest is sent server->client mid-turn when propose_plan is called.
// It carries the full plan content for the client to review. The server blocks
// until the matching PlanApprovalResponse arrives (or deadline/disconnect denies).
type PlanApprovalRequest struct {
	Type       string `json:"type"` // always "plan_approval_request"
	ID         string `json:"id"`
	SessionID  string `json:"session_id,omitempty"`
	TurnID     string `json:"turn_id,omitempty"`
	PlanID     string `json:"plan_id"`
	Title      string `json:"title"`
	Content    string `json:"content"`
	DeadlineMS int64  `json:"deadline_ms,omitempty"`
}

// PlanApprovalResponse is the client's verdict on a proposed plan.
type PlanApprovalResponse struct {
	Type     string `json:"type"` // always "plan_approval_response"
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
}

// NewApprovalRequest builds a request, reusing toWireArgs so tool_args is
// structured JSON on the wire exactly like it is on tool_started events.
func NewApprovalRequest(id, sessionID, turnID, toolName, toolArgs string, deadlineMS int64) ApprovalRequest {
	return ApprovalRequest{
		Type:       "approval_request",
		ID:         id,
		SessionID:  sessionID,
		TurnID:     turnID,
		ToolName:   toolName,
		ToolArgs:   toWireArgs(toolArgs),
		DeadlineMS: deadlineMS,
	}
}
