package server

import (
	"context"

	"code-agent/internal/agent"
)

// Subscriber is the slice of a conversation the bridge consumes: a live event
// channel plus an unsubscribe. *conversation.Conversation satisfies it, so the
// server depends only on this interface and agent.Event — never on the
// conversation package itself (keeps the dependency one-way: Layer 2 -> Layer 1).
type Subscriber interface {
	Subscribe() (<-chan agent.Event, func())
}

// Session is the full handle a WS connection drives, composed from the three
// narrow views its sub-components depend on: Subscriber (the Bridge streams it
// out), CommandTarget (the Router drives turns / cancels), plus the approver
// wiring the handler attaches. *conversation.Conversation satisfies it. Keeping
// the views separate means the Bridge and Router each depend only on the slice
// they use, not the whole handle.
type Session interface {
	Subscriber
	CommandTarget
	SetApprover(agent.Approver)
	SetPlanApprover(agent.PlanApprover)
	SetClientToolWaiter(agent.ClientToolWaiter) // v1.1: client tool execution
}

// Bridge streams one conversation's events to one client as encoded agent-wire
// frames. It is the per-client seam a WS/SSE handler builds on: subscribe, send
// the hello handshake, then pump each event through the encoder to the sink.
//
// Unlike StreamEmitter (a push agent.Emitter that swallows send errors so it can
// never break the agent loop), the Bridge pulls from a subscription and treats a
// send failure as "client gone": it returns, and the deferred unsubscribe tears
// the subscription down.
type Bridge struct {
	sink            FrameSink
	parentSessionID string
	capabilities    []string
}

// NewBridge wires a bridge to one client's frame sink.
func NewBridge(sink FrameSink) *Bridge { return &Bridge{sink: sink} }

// WithParent stamps parent_session_id on every frame (a subagent sub-stream).
func (b *Bridge) WithParent(parentSessionID string) *Bridge {
	b.parentSessionID = parentSessionID
	return b
}

// WithCapabilities sets the server capabilities declared in the hello handshake.
func (b *Bridge) WithCapabilities(caps []string) *Bridge {
	b.capabilities = caps
	return b
}

// Run subscribes, sends the hello frame (pinning the protocol version once), then
// pumps events to the sink as frames until the subscription closes or ctx is
// done. Subscription happens before the hello send, so no event emitted during
// the handshake is missed. A malformed event is skipped (one bad event must not
// kill the stream); a sink error stops the stream. It always unsubscribes on
// return.
func (b *Bridge) Run(ctx context.Context, sub Subscriber, serverName string) error {
	events, unsub := sub.Subscribe()
	defer unsub()

	hello, err := Hello(serverName, b.capabilities)
	if err != nil {
		return err
	}
	if err := b.sink.Send(hello); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-events:
			if !ok {
				return nil // the conversation closed the subscription
			}
			frame, err := Encode(e, newEventID(), b.parentSessionID)
			if err != nil {
				continue
			}
			if err := b.sink.Send(frame); err != nil {
				return err // client gone
			}
		}
	}
}
