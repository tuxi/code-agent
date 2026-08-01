package controlplane

import (
	"context"
	"encoding/json"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/tools"
)

type RuntimeTarget struct {
	executor *conversation.TurnExecutor
	events   conversation.ConversationEventStore
}

func NewRuntimeTarget(executor *conversation.TurnExecutor, events conversation.ConversationEventStore) *RuntimeTarget {
	return &RuntimeTarget{executor: executor, events: events}
}

func (t *RuntimeTarget) Accept(ctx, executionCtx context.Context, sessionID string, request tools.SessionSendRequest) (tools.SessionDelivery, error) {
	admission, err := t.executor.AcceptCrossSessionMessage(ctx, executionCtx, sessionID, conversation.CrossSessionEnvelope{
		Text: request.Message, Model: request.Model, SenderSessionID: request.SenderSessionID,
		SenderTurnID: request.SenderTurnID, MessageID: request.MessageID,
		CorrelationID: request.CorrelationID, Intent: request.Intent,
	})
	if err != nil {
		return tools.SessionDelivery{}, err
	}
	return tools.SessionDelivery{Accepted: true, Delivery: admission.Delivery, SessionID: sessionID, TurnID: admission.TurnID, Cursor: admission.Cursor}, nil
}

func (t *RuntimeTarget) EventsSince(ctx context.Context, sessionID, turnID string, cursor int64) (*tools.SessionWaitCompletion, int64, error) {
	records, err := t.events.ReplaySince(ctx, sessionID, cursor)
	if err != nil {
		return nil, cursor, err
	}
	latest := cursor
	for _, record := range records {
		if record.Seq > latest {
			latest = record.Seq
		}
		if record.TurnID != turnID {
			continue
		}
		status := ""
		switch agent.EventKind(record.Kind) {
		case agent.EventTurnFinished:
			status = "completed"
		case agent.EventTurnFailed:
			status = "failed"
		case agent.EventTurnCancelled:
			status = "cancelled"
		}
		if status != "" {
			payload := append(json.RawMessage(nil), record.Payload...)
			return &tools.SessionWaitCompletion{SessionID: sessionID, TurnID: turnID, Status: status, Cursor: record.Seq, Event: payload}, latest, nil
		}
	}
	return nil, latest, nil
}
