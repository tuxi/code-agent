package conversation

import (
	"context"

	"code-agent/internal/agent"
	"code-agent/internal/model"
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
func (s *TransportSession) SendMessage(ctx context.Context, text string, model string) (agent.TurnResult, error) {
	return s.ex.Execute(ctx, s.id, text, model)
}

// SendMessageWithAssets is the asset-first command-plane extension. It leaves
// the legacy CommandTarget method untouched for older clients.
func (s *TransportSession) SendMessageWithAssets(ctx context.Context, text, modelName string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	return s.ex.ExecuteWithAssets(ctx, s.id, text, modelName, assets)
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

// SetClientToolWaiter wires the WS connection's remote tool-result waiter. Satisfies server.Session.
func (s *TransportSession) SetClientToolWaiter(w agent.ClientToolWaiter) {
	s.ex.SetClientToolWaiter(s.id, w)
}

// RegisterTools stores client-side tool definitions. Satisfies server.CommandTarget.
func (s *TransportSession) RegisterTools(tools []agent.ClientToolDef) {
	s.ex.RegisterTools(s.id, tools)
}
