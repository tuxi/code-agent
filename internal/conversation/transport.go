package conversation

import (
	"context"
	"database/sql"
	"errors"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
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
func (s *TransportSession) SendMessage(ctx context.Context, text string, model string, effort string) (agent.TurnResult, error) {
	return s.ex.Execute(ctx, s.id, text, model, effort)
}

// SendMessageWithAssets is the asset-first command-plane extension. It leaves
// the legacy CommandTarget method untouched for older clients.
func (s *TransportSession) SendMessageWithAssets(ctx context.Context, text, model, effort string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	return s.ex.ExecuteWithAssets(ctx, s.id, text, model, effort, assets)
}

func (s *TransportSession) SendMessageWithRequestID(ctx context.Context, requestID, text, model, effort string) (agent.TurnResult, error) {
	return s.ex.ExecuteWithRequestID(ctx, s.id, requestID, text, model, effort)
}

func (s *TransportSession) SendMessageWithRequestIDAndAssets(ctx context.Context, requestID, text, model, effort string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	return s.ex.ExecuteWithRequestIDAndAssets(ctx, s.id, requestID, text, model, effort, assets)
}

func (s *TransportSession) SendMessageWithRequestIDAndAllAssets(ctx context.Context, requestID, text, model, effort string, assets []model.GatewayAssetRef, localAssets []model.LocalAssetRef) (agent.TurnResult, error) {
	return s.ex.ExecuteWithRequestIDAndAllAssets(ctx, s.id, requestID, text, model, effort, assets, localAssets)
}

// Cancel stops the in-flight turn. Satisfies server.CommandTarget.
func (s *TransportSession) Cancel() {
	s.ex.Cancel(s.id)
}

func (s *TransportSession) CancelTurn(ctx context.Context, turnID string) error {
	return s.ex.CancelTurn(ctx, s.id, turnID)
}

func (s *TransportSession) BackgroundContinuationEnabled(ctx context.Context) bool {
	if s.ex.continuationStore() == nil {
		return false
	}
	provider, ok := s.ex.repo.(session.RuntimeInputProvider)
	if !ok || provider.RuntimeInputStore() == nil {
		return false
	}
	sess, err := s.ex.repo.Load(ctx, s.id)
	if err != nil || !sess.ArchivedAt.IsZero() {
		return false
	}
	builder, ok := s.ex.rb.(interface{ BackgroundContinuationConfigured(string) bool })
	return ok && builder.BackgroundContinuationConfigured(sess.WorkspacePath)
}
func (s *TransportSession) SetBackgroundContinuationSupport(enabled bool) {
	s.ex.SetBackgroundContinuationSupport(s.id, enabled)
}
func (s *TransportSession) BackgroundContinuationState(ctx context.Context) (string, *agent.BackgroundContinuationInfo, error) {
	store := s.ex.continuationStore()
	if store == nil {
		return "", nil, nil
	}
	c, err := store.Continuation(ctx, s.id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if len(c.Checkpoint) == 0 {
		return "", nil, nil
	}
	info := backgroundInfo(c)
	if c.State == "active" {
		s.ex.backgroundMu.Lock()
		live := false
		for _, attempt := range s.ex.backgroundAttempts {
			live = live || attempt.sessionID == s.id && attempt.turnID == c.TurnID
		}
		s.ex.backgroundMu.Unlock()
		if !live {
			info.State = "needs_attention"
			info.Reason = "background continuation execution cannot be confirmed"
		}
	}
	return c.TurnID, info, nil
}

// SetApprover wires the WS connection's remote approver. Satisfies server.Session.
func (s *TransportSession) SetApprover(a agent.Approver) {
	s.ex.SetApprover(s.id, a)
}

// SetPlanApprover wires the WS connection's remote plan approver. Satisfies server.Session.
func (s *TransportSession) SetPlanApprover(pa agent.PlanApprover) {
	s.ex.SetPlanApprover(s.id, pa)
}

// SetAskUserApprover wires the WS connection's remote ask_user approver. Satisfies server.Session.
func (s *TransportSession) SetAskUserApprover(aa agent.AskUserApprover) {
	s.ex.SetAskUserApprover(s.id, aa)
}

// SetClientToolWaiter wires the WS connection's remote tool-result waiter. Satisfies server.Session.
func (s *TransportSession) SetClientToolWaiter(w agent.ClientToolWaiter) {
	s.ex.SetClientToolWaiter(s.id, w)
}

// RegisterTools stores client-side tool definitions. Satisfies server.CommandTarget.
func (s *TransportSession) RegisterTools(tools []agent.ClientToolDef) {
	s.ex.RegisterTools(s.id, tools)
}
