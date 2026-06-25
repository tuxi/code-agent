package server

import (
	"context"
	"encoding/json"

	"code-agent/internal/agent"
)

// CommandTarget is the command plane: the actions a client triggers on a session.
// It is the narrow slice of a Session the Router needs — no streaming, no approver
// wiring — so the routing layer can be unit-tested against a fake and reused by
// any transport.
type CommandTarget interface {
	SendMessage(ctx context.Context, text string) (agent.TurnResult, error)
	Cancel()
}

// ApprovalResolver is the control plane: deliver a client's approval verdict to
// the blocked Approve call. *RemoteApprover satisfies it.
type ApprovalResolver interface {
	Resolve(id string, approved bool)
}

// Router decodes one inbound client message and routes it to the command plane or
// the control plane. It is transport-agnostic: a WebSocket read loop, an SSE POST
// handler, or an in-process iOS bridge all feed it raw frames, so the
// command/control split lives here once instead of being re-implemented (and
// re-bugged) in every transport.
//
// Route returns immediately. A send_message turn runs on its own goroutine: the
// turn blocks for its whole duration and a side-effecting tool inside it blocks on
// the approval round-trip, so running it inline would stall whatever loop feeds
// the Router — and deadlock the very approval_response it is waiting for. Unknown
// message types are ignored (forward-compatibility); a nil target is a no-op.
type Router struct {
	Commands  CommandTarget
	Approvals ApprovalResolver
}

// Route dispatches one raw inbound message.
func (r Router) Route(ctx context.Context, data []byte) {
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &env) != nil {
		return
	}
	switch env.Type {
	case MsgTypeSendMessage:
		var m SendMessage
		if json.Unmarshal(data, &m) == nil && r.Commands != nil {
			go func() { _, _ = r.Commands.SendMessage(ctx, m.Text) }()
		}
	case MsgTypeCancelTurn:
		if r.Commands != nil {
			r.Commands.Cancel()
		}
	case MsgTypeApprovalResponse:
		var m ApprovalResponse
		if json.Unmarshal(data, &m) == nil && r.Approvals != nil {
			r.Approvals.Resolve(m.ID, m.Approved)
		}
	}
}
