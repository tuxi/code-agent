package sqlite

import (
	"context"
	"errors"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

func TestUserQueueAdmissionOriginalIdentityAndReopen(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	advance, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	c := advance.Claim
	accepted, created, seq, err := q.ReserveUserQueueClaim(ctx, "owner", "a", c.Message.Input.Context)
	if err != nil || !created || seq == 0 || accepted.RequestID != c.ExecutionRequestID || accepted.TurnID != c.TurnID || accepted.State != session.TurnInputAccepted {
		t.Fatal(accepted, created, seq, err)
	}
	if err = s.reopen(); err != nil {
		t.Fatal(err)
	}
	q, err = NewUserQueue(s)
	if err != nil {
		t.Fatal(err)
	}
	again, created, seq2, err := q.ReserveUserQueueClaim(ctx, "owner", "a", c.Message.Input.Context)
	if err != nil || created || again.TurnID != accepted.TurnID || seq2 != seq {
		t.Fatal(again, created, seq2, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM session_events WHERE turn_id=? AND kind='turn_accepted'`, accepted.TurnID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	page, err := q.ListUserQueue(ctx, "owner", nil, 0)
	if err != nil || len(page.Items) != 2 || page.Items[0].State != "claimed" {
		t.Fatal(page, err)
	}
}

func TestUserQueueAdmissionContextChangePauses(t *testing.T) {
	for _, field := range []string{"model", "effort", "permission", "scope", "workspace"} {
		t.Run(field, func(t *testing.T) {
			s, q, in := queueDeliveryFixture(t)
			ctx := context.Background()
			advance, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
			if err != nil {
				t.Fatal(err)
			}
			current := advance.Claim.Message.Input.Context
			switch field {
			case "model":
				current.ResolvedModel = "other"
			case "effort":
				current.ReasoningEffort = "low"
			case "permission":
				current.ApprovalMode = "full"
			case "scope":
				current.AuthorizationScope = "other"
			case "workspace":
				if _, err = s.db.Exec(`UPDATE sessions SET workspace_path='/changed' WHERE id='owner'`); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, _, err = q.ReserveUserQueueClaim(ctx, "owner", "a", current); !errors.Is(err, session.ErrUserQueueContextChanged) {
				t.Fatal(err)
			}
			page, err := q.ListUserQueue(ctx, "owner", nil, 0)
			if err != nil || page.State != "paused" || page.PauseReason != "execution_context_changed" || len(page.Items) != 2 {
				t.Fatal(page, err)
			}
			var count int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM turn_inputs`).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestUserQueueAdmissionFailureRollsBack(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	advance, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_queue_accept BEFORE INSERT ON session_events WHEN NEW.kind='turn_accepted' BEGIN SELECT RAISE(ABORT,'accept failed'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = q.ReserveUserQueueClaim(ctx, "owner", "a", advance.Claim.Message.Input.Context); err == nil {
		t.Fatal("expected rollback")
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM turn_inputs`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_queue_accept`); err != nil {
		t.Fatal(err)
	}
	accepted, created, _, err := q.ReserveUserQueueClaim(ctx, "owner", "a", advance.Claim.Message.Input.Context)
	if err != nil || !created || accepted.RequestID != advance.Claim.ExecutionRequestID {
		t.Fatal(accepted, created, err)
	}
}

func TestUserQueueStartedReceiptFailureRollsBack(t *testing.T) {
	s, q, in := queueDeliveryFixture(t)
	ctx := context.Background()
	advance, err := q.FinishUserQueueTurn(ctx, queueEnd(in, session.TurnInputCompleted))
	if err != nil {
		t.Fatal(err)
	}
	frozen := advance.Claim.Message.Input.Context
	accepted, _, _, err := q.ReserveUserQueueClaim(ctx, "owner", "a", frozen)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	before := len(sess.Messages)
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: accepted.Text, OriginTurnID: accepted.TurnID})
	if _, err = s.db.Exec(`CREATE TRIGGER reject_queue_started BEFORE INSERT ON session_events WHEN NEW.kind='user_queue_started' BEGIN SELECT RAISE(ABORT,'started failed'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err = q.StartUserQueueClaim(ctx, accepted, sess, frozen); err == nil {
		t.Fatal("expected rollback")
	}
	stored, err := s.TurnInput(ctx, "owner", accepted.RequestID)
	if err != nil || stored.State != session.TurnInputAccepted {
		t.Fatal(stored.State, err)
	}
	reloaded, err := s.Load(ctx, "owner")
	if err != nil || len(reloaded.Messages) != before {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_queue_started`); err != nil {
		t.Fatal(err)
	}
	seq, err := q.StartUserQueueClaim(ctx, accepted, sess, frozen)
	if err != nil || seq == 0 {
		t.Fatal(seq, err)
	}
	if _, err = q.StartUserQueueClaim(ctx, accepted, sess, frozen); !errors.Is(err, session.ErrUserQueueUnavailable) {
		t.Fatal(err)
	}
}
