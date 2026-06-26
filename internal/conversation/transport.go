package conversation

import (
	"context"

	"code-agent/internal/agent"
)

// TransportSession is a thin adapter that satisfies server.Session (Subscriber
// + CommandTarget + SetApprover) by delegating to TurnExecutor. WS handler's
// Resolve function creates one per connection — no map lookup, no long-lived
// Conversation object.
type TransportSession struct {
	id string
	ex *TurnExecutor
}

// NewTransportSession creates a session handle that delegates to the executor.
func NewTransportSession(sessionID string, executor *TurnExecutor) *TransportSession {
	return &TransportSession{id: sessionID, ex: executor}
}

// Subscribe returns a live event channel for the session. Satisfies server.Subscriber.
func (s *TransportSession) Subscribe() (<-chan agent.Event, func()) {
	return s.ex.subs.Subscribe(s.id)
}

// SendMessage drives one turn. Satisfies server.CommandTarget.
func (s *TransportSession) SendMessage(ctx context.Context, text string) (agent.TurnResult, error) {
	return s.ex.Execute(ctx, s.id, text)
}

// Cancel stops the in-flight turn. Satisfies server.CommandTarget.
func (s *TransportSession) Cancel() {
	s.ex.Cancel(s.id)
}

// SetApprover wires the WS connection's remote approver. Satisfies server.Session.
func (s *TransportSession) SetApprover(a agent.Approver) {
	s.ex.SetApprover(s.id, a)
}

// SetPlanApprover wires the WS connection's remote plan approver. Satisfies server.Session.
func (s *TransportSession) SetPlanApprover(pa agent.PlanApprover) {
	s.ex.SetPlanApprover(s.id, pa)
}
