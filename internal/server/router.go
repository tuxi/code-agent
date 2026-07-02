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
	RegisterTools(tools []agent.ClientToolDef)
}

// ApprovalResolver is the control plane: deliver a client's approval verdict to
// the blocked Approve call. *RemoteApprover satisfies it.
type ApprovalResolver interface {
	Resolve(id string, approved bool)
}

// ToolResultResolver delivers a client-tool-execution result to the blocked
// agent.ClientToolWaiter.Wait call. *RemoteToolResultWaiter satisfies it.
type ToolResultResolver interface {
	Deliver(callID string, result agent.ToolCallResult)
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
	Commands    CommandTarget
	Approvals   ApprovalResolver
	ToolResults ToolResultResolver // v1.1: delivers client tool results to the blocked waiter
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
			// The turn outlives the connection that started it: switching
			// conversations closes the WS, and cancelling here would kill an
			// in-flight turn (and any tool the user later approves). Turn
			// lifetime is owned by the session registry — cancel_turn,
			// SuspendAll, Shutdown — never by the transport.
			turnCtx := context.WithoutCancel(ctx)
			go func() { _, _ = r.Commands.SendMessage(turnCtx, m.Text) }()
		}
	case MsgTypeRegisterTools:
		var m RegisterTools
		if json.Unmarshal(data, &m) == nil && r.Commands != nil {
			r.Commands.RegisterTools(m.Tools)
		}
	case MsgTypeAgentInput:
		var m AgentInput
		if json.Unmarshal(data, &m) != nil {
			return
		}
		switch m.Kind {
		case "text":
			if r.Commands != nil {
				// Same detachment as send_message: the turn must survive this
				// connection closing.
				turnCtx := context.WithoutCancel(ctx)
				go func() { _, _ = r.Commands.SendMessage(turnCtx, m.Text) }()
			}
		case "tool_result":
			if m.ToolResult != nil && r.ToolResults != nil {
				r.ToolResults.Deliver(m.ToolResult.ToolUseID, agent.ToolCallResult{
					Subtype: m.ToolResult.Subtype,
					Content: m.ToolResult.Content,
					Output:  m.ToolResult.Output,
					Assets:  m.ToolResult.Assets,
					IsError: m.ToolResult.IsError,
				})
			}
		case "command":
			if m.Text == "cancel" && r.Commands != nil {
				r.Commands.Cancel()
			}
			// switch_model、goal_start are reserved for a future version.
		case "system":
			// v1.1 stub: parsed but not acted on.
			// patch_context、update_memory、override_plan semantics are deferred.
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
	case MsgTypePlanApprovalResponse:
		var m PlanApprovalResponse
		if json.Unmarshal(data, &m) == nil && r.Approvals != nil {
			r.Approvals.Resolve(m.ID, m.Approved)
		}
	}
}
