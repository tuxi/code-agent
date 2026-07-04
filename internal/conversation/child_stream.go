package conversation

import "code-agent/internal/agent"

// ChildStreamSubscriber is a read-only live subscription to a stream id that is
// NOT a conversation you can drive — a background job's own event stream (P8.7
// Phase C, GET /v1/jobs/{id}/stream). Unlike TransportSession it carries no
// command plane (no SendMessage, no approver, no client-tool waiter): a child
// stream is observe-only. It shares the SAME SubscriptionManager, keyed on the
// id, so the live events the runtime's job sink fans to that id reach the
// client. It satisfies server.Subscriber (Subscribe only).
type ChildStreamSubscriber struct {
	id string
	ex *TurnExecutor
}

// NewChildStreamSubscriber returns a read-only subscriber for the given stream
// id (a job id), delegating to the executor's subscription manager.
func NewChildStreamSubscriber(id string, ex *TurnExecutor) *ChildStreamSubscriber {
	return &ChildStreamSubscriber{id: id, ex: ex}
}

// Subscribe returns a live event channel for the child stream.
func (s *ChildStreamSubscriber) Subscribe() (<-chan agent.Event, func()) {
	return s.ex.subs.Subscribe(s.id)
}
