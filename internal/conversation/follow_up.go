package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

// FollowUpError is a pre-accept control error for a stale, unknown, or malformed
// follow-up interaction. It maps to agent_input_rejected on the wire.
type FollowUpError struct {
	Code    string
	Message string
}

func (e FollowUpError) Error() string               { return e.Message }
func (e FollowUpError) AgentInputErrorCode() string { return e.Code }
func (e FollowUpError) LifecycleErrorCode() string  { return e.Code }
func (e FollowUpError) SafeMessage() string         { return e.Message }

func staleFollowUp(message string) error {
	return FollowUpError{Code: "follow_up_stale", Message: message}
}

func cloneFollowUpReply(reply *session.FollowUpReply) *session.FollowUpReply {
	if reply == nil {
		return nil
	}
	copy := *reply
	return &copy
}

func (e *TurnExecutor) turnInputExists(ctx context.Context, sessionID, requestID string) bool {
	if requestID == "" {
		return false
	}
	repo, ok := e.repo.(TurnInputRepository)
	if !ok {
		return false
	}
	_, err := repo.TurnInput(ctx, sessionID, requestID)
	return err == nil
}

func (e *TurnExecutor) validateFollowUpReply(ctx context.Context, sessionID, text string, reply *session.FollowUpReply) error {
	if reply == nil {
		return nil
	}
	active, err := e.activeFollowUp(ctx, sessionID)
	if err != nil {
		return err
	}
	if active == nil || active.TurnID != reply.TurnID || active.FollowUp.ID != reply.FollowUpID {
		return staleFollowUp("The selected follow-up is no longer active.")
	}
	if reply.OptionID == "" {
		return FollowUpError{Code: "invalid_follow_up", Message: "A follow-up selection requires option_id."}
	}
	selectedMessage := ""
	found := false
	for i := range active.FollowUp.Options {
		if active.FollowUp.Options[i].ID == reply.OptionID {
			selectedMessage = active.FollowUp.Options[i].Message
			found = true
			break
		}
	}
	if !found {
		return FollowUpError{Code: "invalid_follow_up", Message: "The selected follow-up option does not exist."}
	}
	if strings.TrimSpace(text) != selectedMessage {
		return FollowUpError{Code: "invalid_follow_up", Message: "The submitted text does not match the selected follow-up option."}
	}
	return nil
}

func (e *TurnExecutor) activeFollowUp(ctx context.Context, sessionID string) (*agent.Event, error) {
	records, err := e.events.Replay(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var active *agent.Event
	for _, record := range records {
		switch record.Kind {
		case string(agent.EventTurnAccepted), string(agent.EventTurnStarted):
			// A newer submission means the previous terminal card is no longer the
			// active tail, even if its new turn has not completed yet.
			active = nil
		case string(agent.EventTurnFinished):
			var event agent.Event
			if json.Unmarshal(record.Payload, &event) != nil {
				continue
			}
			if event.FollowUp != nil {
				event.Seq = record.Seq
				active = &event
			} else {
				active = nil
			}
		case string(agent.EventTurnFailed), string(agent.EventTurnCancelled):
			active = nil
		case string(agent.EventFollowUpDismissed):
			var event agent.Event
			if json.Unmarshal(record.Payload, &event) == nil && active != nil && event.FollowUpID == active.FollowUp.ID {
				active = nil
			}
		}
	}
	return active, nil
}

func (e *TurnExecutor) followUpDismissed(ctx context.Context, sessionID, turnID, followUpID string) bool {
	records, err := e.events.Replay(ctx, sessionID)
	if err != nil {
		return false
	}
	for _, record := range records {
		if record.Kind != string(agent.EventFollowUpDismissed) || record.TurnID != turnID {
			continue
		}
		var event agent.Event
		if json.Unmarshal(record.Payload, &event) == nil && event.FollowUpID == followUpID {
			return true
		}
	}
	return false
}

// DismissFollowUp persists a non-turn UI action. The same event is replayed to
// every client, so closing a card survives reconnects and multiple devices.
func (e *TurnExecutor) DismissFollowUp(ctx context.Context, sessionID string, reply session.FollowUpReply) error {
	release, err := e.beginSessionTurn(sessionID)
	if err != nil {
		return err
	}
	defer release()

	sess, err := e.repo.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	if !sess.ArchivedAt.IsZero() {
		return ErrConversationArchived
	}
	active, err := e.activeFollowUp(ctx, sessionID)
	if err != nil {
		return err
	}
	if active == nil || active.TurnID != reply.TurnID || active.FollowUp.ID != reply.FollowUpID {
		if e.followUpDismissed(ctx, sessionID, reply.TurnID, reply.FollowUpID) {
			return nil
		}
		return staleFollowUp("The follow-up is no longer active.")
	}
	if !active.FollowUp.Dismissible {
		return FollowUpError{Code: "invalid_follow_up", Message: "This follow-up cannot be dismissed."}
	}

	event := agent.Event{
		Kind:       agent.EventFollowUpDismissed,
		SessionID:  sessionID,
		TurnID:     active.TurnID,
		FollowUpID: active.FollowUp.ID,
		At:         time.Now(),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	seq, err := e.events.Append(ctx, session.EventRecord{
		SessionID: sessionID,
		TurnID:    active.TurnID,
		Kind:      string(agent.EventFollowUpDismissed),
		At:        event.At,
		Payload:   payload,
	})
	if err != nil {
		return fmt.Errorf("persist follow-up dismissal: %w", err)
	}
	event.Seq = seq
	e.subs.Emitter(sessionID).Emit(event)
	return nil
}
