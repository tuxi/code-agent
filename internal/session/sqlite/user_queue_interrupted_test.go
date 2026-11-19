package sqlite

import (
	"code-agent/internal/model"
	"code-agent/internal/session"
	"context"
	"errors"
	"testing"
)

func interruptedQueueFixture(t *testing.T) (*Store, *UserQueue, session.TurnInput, session.QueueExecutionContext) {
	t.Helper()
	s, q, first := queueDeliveryFixture(t)
	ctx := context.Background()
	next, err := q.FinishUserQueueTurn(ctx, queueEnd(first, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	frozen := next.Claim.Message.Input.Context
	input, _, _, err := q.ReserveUserQueueClaim(ctx, first.SessionID, "a", frozen)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Load(ctx, first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: input.Text, OriginTurnID: input.TurnID})
	if _, err = q.StartUserQueueClaim(ctx, input, sess, frozen); err != nil {
		t.Fatal(err)
	}
	if err = s.PauseInterruptedSessions(ctx); err != nil {
		t.Fatal(err)
	}
	paused, err := q.ReconcileUserQueue(ctx, input.SessionID)
	if err != nil || paused.PauseReason != "execution_interrupted" {
		t.Fatal(paused, err)
	}
	return s, q, input, frozen
}

func TestInterruptedQueueResumeRequiresExplicitCurrentRevision(t *testing.T) {
	s, q, input, frozen := interruptedQueueFixture(t)
	ctx := context.Background()
	if _, _, err := q.PendingInterruptedUserQueue(ctx, input.SessionID); err == nil {
		t.Fatal("resume was automatic")
	}
	page, err := q.ListUserQueue(ctx, input.SessionID, nil, 0)
	if err != nil || page.Interrupted == nil || page.Interrupted.Input.RequestID != "a" {
		t.Fatal(page, err)
	}
	change := session.UserQueueControl{Action: "resume_interrupted", RequestID: "a", Revision: &page.Revision}
	if err = q.ControlUserQueue(ctx, input.SessionID, "wrong-owner", change); err == nil {
		t.Fatal("wrong principal accepted")
	}
	if err = q.ControlUserQueue(ctx, input.SessionID, "local-owner", change); err != nil {
		t.Fatal(err)
	}
	if err = q.ControlUserQueue(ctx, input.SessionID, "local-owner", change); !errors.Is(err, session.ErrUserQueueRevision) {
		t.Fatal("old control was accepted", err)
	}
	if err = s.reopen(); err != nil {
		t.Fatal(err)
	}
	q, err = NewUserQueue(s)
	if err != nil {
		t.Fatal(err)
	}
	recovered, selection, err := q.PendingInterruptedUserQueue(ctx, input.SessionID)
	if err != nil || recovered.RequestID != input.RequestID || recovered.TurnID != input.TurnID || selection != frozen {
		t.Fatal(recovered, selection, err)
	}
	sess, err := s.Load(ctx, input.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	changed := frozen
	changed.ApprovalMode = "full"
	if _, err = q.StartInterruptedUserQueue(ctx, input, sess, changed); !errors.Is(err, session.ErrUserQueueContextChanged) {
		t.Fatal(err)
	}
	if _, err = q.StartInterruptedUserQueue(ctx, input, sess, frozen); err != nil {
		t.Fatal(err)
	}
	if _, err = q.StartInterruptedUserQueue(ctx, input, sess, frozen); err == nil {
		t.Fatal("duplicate resume started")
	}
	var users int
	for _, m := range sess.Messages {
		if m.Role == model.RoleUser && m.OriginTurnID == input.TurnID {
			users++
		}
	}
	if users != 1 {
		t.Fatal("duplicate user message", users)
	}
	out, err := q.FinishUserQueueTurn(ctx, queueEnd(input, session.TurnInputCompleted))
	if err != nil || out.Claim == nil || out.Claim.Message.Input.RequestID != "b" {
		t.Fatal(out, err)
	}
}

func TestInterruptedQueueResumeCheckpointFailureRollsBack(t *testing.T) {
	s, q, input, frozen := interruptedQueueFixture(t)
	ctx := context.Background()
	page, err := q.ListUserQueue(ctx, input.SessionID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.ControlUserQueue(ctx, input.SessionID, "local-owner", session.UserQueueControl{Action: "resume_interrupted", RequestID: "a", Revision: &page.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_queue_resume BEFORE INSERT ON session_events WHEN NEW.kind='user_queue_resumed' BEGIN SELECT RAISE(ABORT,'resume write failed'); END`); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Load(ctx, input.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.StartInterruptedUserQueue(ctx, input, sess, frozen); err == nil {
		t.Fatal("expected rollback")
	}
	if sess.TurnStatus() != session.TurnStatusPaused {
		t.Fatal("caller state changed", sess.TurnStatus())
	}
	page, err = q.ListUserQueue(ctx, input.SessionID, nil, 0)
	if err != nil || page.PauseReason != "resume_requested" || page.Interrupted == nil {
		t.Fatal(page, err)
	}
}

func TestInterruptedQueueIgnoresTerminalEventFromBeforeResume(t *testing.T) {
	s, q, input, frozen := interruptedQueueFixture(t)
	ctx := context.Background()
	end := queueEnd(input, session.TurnInputFailed)
	if _, err := s.db.Exec(`INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,?,?,?)`, end.SessionID, end.TurnID, end.Event.Kind, formatTime(end.Event.At), string(end.Event.Payload)); err != nil {
		t.Fatal(err)
	}
	page, err := q.ListUserQueue(ctx, input.SessionID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.ControlUserQueue(ctx, input.SessionID, "local-owner", session.UserQueueControl{
		Action: "resume_interrupted", RequestID: "a", Revision: &page.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Load(ctx, input.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.StartInterruptedUserQueue(ctx, input, sess, frozen); err != nil {
		t.Fatal(err)
	}
	if err = s.PauseInterruptedSessions(ctx); err != nil {
		t.Fatal(err)
	}
	advance, err := q.ReconcileUserQueue(ctx, input.SessionID)
	if err != nil || advance.Claim != nil || advance.PauseReason != "execution_interrupted" {
		t.Fatal(advance, err)
	}
	stored, err := s.TurnInput(ctx, input.SessionID, input.RequestID)
	if err != nil || stored.State != session.TurnInputRunning {
		t.Fatal(stored, err)
	}
}
