package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"code-agent/internal/session"
)

func queueDeliveryFixture(t *testing.T) (*Store, *UserQueue, session.TurnInput) {
	t.Helper()
	s, q := queueFixture(t)
	ctx := context.Background()
	sess, err := s.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	in, _, _, err := s.ReserveTurnInput(ctx, session.TurnInput{SessionID: "owner", RequestID: "initial", TurnID: "target", PayloadHash: "first", Text: "task"}, session.EventRecord{SessionID: "owner", TurnID: "target", Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartTurnInput(ctx, in, sess); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, _, err = q.EnqueueUserMessage(ctx, queueMessage(id)); err != nil {
			t.Fatal(err)
		}
	}
	return s, q, in
}
func queueEnd(in session.TurnInput, state session.TurnInputState) session.UserQueueTurnEnd {
	now := time.Now().UTC()
	kind := queueTerminalKind(state)
	body, _ := json.Marshal(struct {
		Kind, SessionID, TurnID, RequestID string
		At                                 time.Time
	}{kind, in.SessionID, in.TurnID, in.RequestID, now})
	return session.UserQueueTurnEnd{SessionID: in.SessionID, RequestID: in.RequestID, TurnID: in.TurnID, State: state, Event: session.EventRecord{SessionID: in.SessionID, TurnID: in.TurnID, Kind: kind, At: now, Payload: body}}
}

func TestUserQueueFinishClaimAndRecovery(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	waiting, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || waiting.Claim != nil || waiting.BlockedReason != "turn_active" {
		t.Fatal(waiting, err)
	}
	finished, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil || finished.Claim == nil || finished.Claim.Message.Input.RequestID != "a" || finished.TerminalEventSequence == 0 || finished.Revision != 3 {
		t.Fatal(finished, err)
	}
	first := finished.Claim
	if first.ExecutionRequestID == "" || first.TurnID == "" || first.AfterRequestID != in.RequestID || first.EventSequence == 0 {
		t.Fatal(first)
	}
	if err = s.reopen(); err != nil {
		t.Fatal(err)
	}
	q, err = NewUserQueue(s)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || recovered.Claim == nil || recovered.Claim.ExecutionRequestID != first.ExecutionRequestID || recovered.Claim.TurnID != first.TurnID || recovered.Claim.EventSequence != first.EventSequence || recovered.Revision != 3 {
		t.Fatal(recovered, err)
	}
	// A repeated finish must return the same claim and durable terminal event.
	repeated, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil || repeated.Claim == nil || repeated.Claim.TurnID != first.TurnID || repeated.TerminalEventSequence != finished.TerminalEventSequence || repeated.Revision != 3 {
		t.Fatal(repeated, err)
	}
	var claims, ends, inputs int
	s.db.QueryRow(`SELECT COUNT(*) FROM user_queue_claims`).Scan(&claims)
	s.db.QueryRow(`SELECT COUNT(*) FROM session_events WHERE kind='turn_finished'`).Scan(&ends)
	s.db.QueryRow(`SELECT COUNT(*) FROM turn_inputs`).Scan(&inputs)
	if claims != 1 || ends != 1 || inputs != 1 {
		t.Fatal(claims, ends, inputs)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || len(page.Items) != 2 || page.Items[0].State != "claimed" || page.Items[1].State != "queued" {
		t.Fatal(page, err)
	}
}

func TestUserQueueSequentialExecutionBinding(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	first, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	claim := first.Claim
	// Emulate the next batch's execution bridge through the existing durable
	// admission/start APIs, retaining precisely the committed claim identity.
	next, _, _, err := s.ReserveTurnInput(ctx, session.TurnInput{SessionID: "owner", RequestID: claim.ExecutionRequestID, TurnID: claim.TurnID, PayloadHash: "next", Text: claim.Message.Input.Text}, session.EventRecord{SessionID: "owner", TurnID: claim.TurnID, Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartTurnInput(ctx, next, sess); err != nil {
		t.Fatal(err)
	}
	recovered, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || recovered.Claim == nil || recovered.Claim.TurnID != claim.TurnID {
		t.Fatal(recovered, err)
	}
	second, err := q.FinishUserQueueTurn(ctx, queueEnd(next, session.TurnInputCompleted))
	if err != nil || second.Claim == nil || second.Claim.Message.Input.RequestID != "b" || second.Claim.AfterRequestID != next.RequestID || second.Claim.TurnID == claim.TurnID {
		t.Fatal(second, err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || len(page.Items) != 1 || page.Items[0].Input.RequestID != "b" {
		t.Fatal(page, err)
	}
	historical, err := q.QueuedUserMessage(ctx, "owner", "a")
	if err != nil || historical.State != "started" {
		t.Fatal(historical, err)
	}
}

func TestUserQueueFailureAndCancellationPause(t *testing.T) {
	for _, state := range []session.TurnInputState{session.TurnInputFailed, session.TurnInputCancelled} {
		t.Run(string(state), func(t *testing.T) {
			s, q, in := queueDeliveryFixture(t)
			ctx := context.Background()
			out, err := q.FinishUserQueueTurn(ctx, queueEnd(in, state))
			if err != nil || out.Claim != nil || out.State != "paused" || out.PauseReason != string(state) {
				t.Fatal(out, err)
			}
			if err = s.reopen(); err != nil {
				t.Fatal(err)
			}
			q, err = NewUserQueue(s)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := q.ReconcileUserQueue(ctx, "owner")
			if err != nil || recovered.Claim != nil || recovered.State != "paused" || recovered.Revision != out.Revision {
				t.Fatal(recovered, err)
			}
			page, err := q.ListUserQueue(ctx, "owner", nil, 0)
			if err != nil || len(page.Items) != 2 || page.Items[0].State != "queued" {
				t.Fatal(page, err)
			}
			if _, err = q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted)); !errors.Is(err, session.ErrUserQueueTerminalConflict) {
				t.Fatal(err)
			}
		})
	}
}

func TestUserQueueConcurrentTerminalDecision(t *testing.T) {
	for i := 0; i < 16; i++ {
		s, q, in := queueDeliveryFixture(t)
		ctx := context.Background()
		var wg sync.WaitGroup
		var success, cancel session.UserQueueAdvance
		var successErr, cancelErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			success, successErr = q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
		}()
		go func() {
			defer wg.Done()
			cancel, cancelErr = q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCancelled))
		}()
		wg.Wait()
		switch {
		case successErr == nil:
			if !errors.Is(cancelErr, session.ErrUserQueueTerminalConflict) || success.Claim == nil {
				t.Fatal(success, cancelErr)
			}
		case cancelErr == nil:
			if !errors.Is(successErr, session.ErrUserQueueTerminalConflict) || cancel.State != "paused" || cancel.Claim != nil {
				t.Fatal(cancel, successErr)
			}
		default:
			t.Fatal(successErr, cancelErr)
		}
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_events WHERE kind IN ('turn_finished','turn_cancelled')`).Scan(&count); err != nil || count != 1 {
			t.Fatal(count, err)
		}
	}
}

func TestUserQueueClaimFailureRollsBackTerminal(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_queue_claim BEFORE INSERT ON user_queue_claims BEGIN SELECT RAISE(ABORT,'claim write failed'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted)); err == nil {
		t.Fatal("expected rollback")
	}
	stored, err := s.TurnInput(ctx, "owner", in.RequestID)
	if err != nil || stored.State != session.TurnInputRunning {
		t.Fatal(stored.State, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM session_events WHERE kind='turn_finished'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || page.Revision != 2 || page.Items[0].State != "queued" {
		t.Fatal(page, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_queue_claim`); err != nil {
		t.Fatal(err)
	}
	if _, err = q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted)); err != nil {
		t.Fatal(err)
	}
}

func TestUserQueueWaitAndUnsafeInputsBlockCompletion(t *testing.T) {
	for _, which := range []string{"background", "runtime", "supplement", "waiting_state"} {
		t.Run(which, func(t *testing.T) {
			s, q, in := queueDeliveryFixture(t)
			ctx := context.Background()
			var err error
			switch which {
			case "background":
				_, err = s.db.Exec(`UPDATE runtime_continuations SET state='waiting',checkpoint='{}' WHERE session_id='owner'`)
			case "runtime":
				_, err = s.db.Exec(`INSERT INTO runtime_inputs(id,session_id,kind,content) VALUES('result','owner','background_job_completion','{}')`)
			case "supplement":
				var box *SupplementInbox
				box, err = NewSupplementInbox(s)
				if err == nil {
					err = box.OpenSupplementBoundary(ctx, "owner", in.TurnID)
				}
				if err == nil {
					_, _, err = box.AcceptSupplement(ctx, session.Supplement{SessionID: "owner", RequestID: "extra", ExpectedTurnID: in.TurnID, Text: "more"})
				}
			case "waiting_state":
				err = s.SetTurnInputState(ctx, "owner", in.RequestID, session.TurnInputWaitingBackground)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted)); !errors.Is(err, session.ErrUserQueueUnsafeBoundary) {
				t.Fatal(err)
			}
			out, err := q.ReconcileUserQueue(ctx, "owner")
			if err != nil || out.Claim != nil {
				t.Fatal(out, err)
			}
		})
	}
}

func TestUserQueueRecoversDurableFailureAndChecksArchive(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	// Crash window: a durable cancellation event exists, but the inbox state is
	// still running. Reconciliation must pause rather than trust that projection.
	end := queueEnd(in, session.TurnInputCancelled)
	if _, err := s.db.Exec(`INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,?,?,?)`, end.SessionID, end.TurnID, end.Event.Kind, formatTime(end.Event.At), string(end.Event.Payload)); err != nil {
		t.Fatal(err)
	}
	out, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || out.State != "paused" || out.PauseReason != "cancelled" || out.Claim != nil {
		t.Fatal(out, err)
	}
	// Reuse the already-persisted event instead of publishing another terminal.
	out, err = q.FinishUserQueueTurn(ctx, end)
	if err != nil || out.State != "paused" {
		t.Fatal(out, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM session_events WHERE kind='turn_cancelled'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if _, err = s.Archive(ctx, "owner", time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err = q.ReconcileUserQueue(ctx, "owner")
	if err != nil || out.Claim != nil || out.BlockedReason != "session_archived" {
		t.Fatal(out, err)
	}
	if err = s.Delete(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"user_queue_claims", "user_queue_finishes"} {
		if err = s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
}

func TestUserQueueConcurrentReconcileKeepsOneClaim(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	first, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := q.ReconcileUserQueue(ctx, "owner")
			if e != nil || out.Claim == nil || out.Claim.TurnID != first.Claim.TurnID || out.Revision != first.Revision {
				t.Error(out, e)
			}
		}()
	}
	wg.Wait()
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM session_events WHERE kind='user_queue_claimed'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestUserQueueCancelledBeforeStartStaysVisible(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	first, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	claim := first.Claim
	reserved, _, _, err := s.ReserveTurnInput(ctx, session.TurnInput{SessionID: "owner", RequestID: claim.ExecutionRequestID, TurnID: claim.TurnID, PayloadHash: "next", Text: claim.Message.Input.Text}, session.EventRecord{SessionID: "owner", TurnID: claim.TurnID, Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := q.FinishUserQueueTurn(ctx, queueEnd(reserved, session.TurnInputCancelled))
	if err != nil || out.State != "paused" || out.Claim != nil {
		t.Fatal(out, err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || len(page.Items) != 2 || page.Items[0].State != "claimed" || page.Items[1].State != "queued" {
		t.Fatal(page, err)
	}
	if _, err = q.ReconcileUserQueue(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestUserQueueRejectsMismatchedTerminalEnvelope(t *testing.T) {
	_, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	end := queueEnd(in, session.TurnInputCompleted)
	end.Event.Payload = []byte(`{"Kind":"turn_finished","SessionID":"other","TurnID":"target"}`)
	if _, err := q.FinishUserQueueTurn(ctx, end); !errors.Is(err, session.ErrInvalidQueuedMessage) {
		t.Fatal(err)
	}
	end = queueEnd(in, session.TurnInputCompleted)
	end.TurnID = "other"
	if _, err := q.FinishUserQueueTurn(ctx, end); !errors.Is(err, session.ErrInvalidQueuedMessage) {
		t.Fatal(err)
	}
}

func TestUserQueueMissingTerminalAndCompetingAdmissionBlock(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	if err := s.SetTurnInputState(ctx, "owner", in.RequestID, session.TurnInputCompleted); err != nil {
		t.Fatal(err)
	}
	out, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || out.Claim != nil || out.BlockedReason != "terminal_not_persisted" {
		t.Fatal(out, err)
	}
	out, err = q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil || out.Claim == nil {
		t.Fatal(out, err)
	}
	identity := out.Claim.ExecutionRequestID
	_, _, _, err = s.ReserveTurnInput(ctx, session.TurnInput{SessionID: "owner", RequestID: "manual", TurnID: "manual-turn", PayloadHash: "manual", Text: "manual"}, session.EventRecord{SessionID: "owner", TurnID: "manual-turn", Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	out, err = q.ReconcileUserQueue(ctx, "owner")
	if err != nil || out.Claim != nil || out.BlockedReason != "turn_active" {
		t.Fatal(out, err)
	}
	var stored string
	if err = s.db.QueryRow(`SELECT execution_request_id FROM user_queue_claims WHERE session_id='owner' AND active=1`).Scan(&stored); err != nil || stored != identity {
		t.Fatal(stored, err)
	}
}

func TestUserQueuePauseFailureRollsBackCancellation(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_queue_pause BEFORE UPDATE ON user_queue_control WHEN NEW.state='paused' BEGIN SELECT RAISE(ABORT,'pause write failed'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCancelled)); err == nil {
		t.Fatal("expected rollback")
	}
	input, err := s.TurnInput(ctx, "owner", in.RequestID)
	if err != nil || input.State != session.TurnInputRunning {
		t.Fatal(input.State, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM session_events WHERE kind='turn_cancelled'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || page.State != "enabled" || page.Revision != 2 || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
}

func TestUserQueueRecoversTerminalBeforeQueueProjection(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	first, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	claim := first.Claim
	next, _, _, err := s.ReserveTurnInput(ctx, session.TurnInput{SessionID: "owner", RequestID: claim.ExecutionRequestID, TurnID: claim.TurnID, PayloadHash: "next", Text: claim.Message.Input.Text}, session.EventRecord{SessionID: "owner", TurnID: claim.TurnID, Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartTurnInput(ctx, next, sess); err != nil {
		t.Fatal(err)
	}
	sess.SetTurnStatus(session.TurnStatusDone)
	if err = s.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	end := queueEnd(next, session.TurnInputCompleted)
	if _, err = s.db.Exec(`INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,?,?,?)`, end.SessionID, end.TurnID, end.Event.Kind, formatTime(end.Event.At), string(end.Event.Payload)); err != nil {
		t.Fatal(err)
	}
	if err = s.reopen(); err != nil {
		t.Fatal(err)
	}
	q, err = NewUserQueue(s)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || recovered.Claim == nil || recovered.Claim.Message.Input.RequestID != "b" || recovered.Claim.ExecutionRequestID == claim.ExecutionRequestID {
		t.Fatal(recovered, err)
	}
	stored, err := s.TurnInput(ctx, "owner", claim.ExecutionRequestID)
	if err != nil || stored.State != session.TurnInputCompleted {
		t.Fatal(stored.State, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM session_events WHERE turn_id=? AND kind='turn_finished'`, claim.TurnID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestUserQueueInterruptedStartedClaimPauses(t *testing.T) {
	s, q, initial := queueDeliveryFixture(t)
	ctx := context.Background()
	a, err := q.FinishUserQueueTurn(ctx, queueEnd(initial, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	frozen := a.Claim.Message.Input.Context
	input, _, _, err := q.ReserveUserQueueClaim(ctx, "owner", "a", frozen)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.StartUserQueueClaim(ctx, input, sess, frozen); err != nil {
		t.Fatal(err)
	}
	// The runner may have emitted success just before the daemon died.
	end := queueEnd(input, session.TurnInputCompleted)
	if _, err = s.db.Exec(`INSERT INTO session_events(session_id,turn_id,kind,at,payload) VALUES(?,?,?,?,?)`, end.SessionID, end.TurnID, end.Event.Kind, formatTime(end.Event.At), string(end.Event.Payload)); err != nil {
		t.Fatal(err)
	}
	if err = s.reopen(); err != nil {
		t.Fatal(err)
	}
	if err = s.PauseInterruptedSessions(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || out.Claim != nil || out.PauseReason != "execution_interrupted" {
		t.Fatal(out, err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || len(page.Items) != 1 || page.Items[0].Input.RequestID != "b" || page.Items[0].State != "queued" {
		t.Fatal(page, err)
	}
}

func TestUserQueueControlSurvivesReopen(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	paused, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCancelled))
	if err != nil || paused.State != "paused" {
		t.Fatal(paused, err)
	}
	reconnect := func() *UserQueue {
		t.Helper()
		if err := s.reopen(); err != nil {
			t.Fatal(err)
		}
		q, err := NewUserQueue(s)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	q = reconnect()
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || page.State != "paused" || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
	change := session.UserQueueControl{Action: "resume", Revision: &page.Revision}
	if err := q.ControlUserQueue(ctx, "owner", "another-owner", change); err == nil {
		t.Fatal("different execution scope resumed the queue")
	}
	if err := q.ControlUserQueue(ctx, "owner", "local-owner", change); err != nil {
		t.Fatal(err)
	}
	q = reconnect()
	first, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || first.Claim == nil || first.Claim.Message.Input.RequestID != "a" {
		t.Fatal(first, err)
	}
	again, err := q.ReconcileUserQueue(ctx, "owner")
	if err != nil || again.Claim == nil || again.Claim.EventSequence != first.Claim.EventSequence {
		t.Fatal(again, err)
	}
}

func TestUserQueueResumesAfterStoppedContinuationWithSupplements(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "atomic stop"
		if legacy {
			name = "legacy stop recovery"
		}
		t.Run(name, func(t *testing.T) {
			s, q, in := queueDeliveryFixture(t)
			ctx := context.Background()
			sess, err := s.Load(ctx, in.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			box, err := NewSupplementInbox(s)
			if err != nil {
				t.Fatal(err)
			}
			if err = box.OpenSupplementBoundary(ctx, in.SessionID, in.TurnID); err != nil {
				t.Fatal(err)
			}
			if _, _, err = box.AcceptSupplement(ctx, supplement("pending")); err != nil {
				t.Fatal(err)
			}
			if err = s.AdmitContinuationWork(ctx, in.SessionID, in.TurnID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.ParkContinuation(ctx, sess, session.Continuation{SessionID: in.SessionID, TurnID: in.TurnID, Await: []string{"job"}, Binding: []byte(`{}`), Checkpoint: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.EndContinuation(ctx, in.SessionID, in.TurnID, "stopped"); err != nil || !ok {
				t.Fatal(ok, err)
			}
			receipt, err := box.Supplement(ctx, in.SessionID, "pending")
			if err != nil || receipt.State != "cancelled" {
				t.Fatal(receipt, err)
			}
			if legacy {
				if _, err = s.db.Exec(`UPDATE user_supplement_boundaries SET sealed=0; UPDATE user_supplements SET state='pending_apply'`); err != nil {
					t.Fatal(err)
				}
			}
			paused, err := q.ReconcileUserQueue(ctx, in.SessionID)
			if err != nil || paused.State != "paused" || paused.Claim != nil {
				t.Fatal(paused, err)
			}
			receipt, err = box.Supplement(ctx, in.SessionID, "pending")
			if err != nil || receipt.State != "cancelled" {
				t.Fatal(receipt, err)
			}
			if err = q.ControlUserQueue(ctx, in.SessionID, "local-owner", session.UserQueueControl{Action: "resume", Revision: &paused.Revision}); err != nil {
				t.Fatal(err)
			}
			next, err := q.ReconcileUserQueue(ctx, in.SessionID)
			if err != nil || next.Claim == nil || next.Claim.Message.Input.RequestID != "a" {
				t.Fatal(next, err)
			}
		})
	}
}

func TestSupersedingParkedTurnClosesSupplementBoundary(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	sess, err := s.Load(ctx, in.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewSupplementInbox(s)
	if err != nil {
		t.Fatal(err)
	}
	if err = box.OpenSupplementBoundary(ctx, in.SessionID, in.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = box.AcceptSupplement(ctx, supplement("old-pending")); err != nil {
		t.Fatal(err)
	}
	if err = s.AdmitContinuationWork(ctx, in.SessionID, in.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ParkContinuation(ctx, sess, session.Continuation{SessionID: in.SessionID, TurnID: in.TurnID, Await: []string{"job"}, Binding: []byte(`{}`), Checkpoint: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.AdmitContinuationWork(ctx, in.SessionID, "new-turn"); err != nil {
		t.Fatal(err)
	}
	receipt, err := box.Supplement(ctx, in.SessionID, "old-pending")
	if err != nil || receipt.State != "cancelled" {
		t.Fatal(receipt, err)
	}
	// Emulate the legacy supersede path, whose continuation identity is gone.
	if _, err = s.db.Exec(`UPDATE user_supplement_boundaries SET sealed=0; UPDATE user_supplements SET state='pending_apply'`); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ReconcileUserQueue(ctx, in.SessionID); err != nil {
		t.Fatal(err)
	}
	receipt, err = box.Supplement(ctx, in.SessionID, "old-pending")
	if err != nil || receipt.State != "cancelled" {
		t.Fatal(receipt, err)
	}
}
