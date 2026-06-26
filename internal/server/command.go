package server

// Command-plane messages (client -> server): they trigger an action on a session
// and expect no direct reply — the effect shows up on the event stream. The
// control-plane approval messages (request/response RPC) live in control.go.

// Message type discriminators for the command plane.
const (
	MsgTypeSendMessage           = "send_message"
	MsgTypeCancelTurn            = "cancel_turn"
	MsgTypePlanApprovalResponse  = "plan_approval_response"
)

// SendMessage drives one turn. Text is the user input.
type SendMessage struct {
	Type string `json:"type"` // always "send_message"
	Text string `json:"text"`
}

// CancelTurn cancels the in-flight turn at the next checkpoint.
type CancelTurn struct {
	Type string `json:"type"` // always "cancel_turn"
}
