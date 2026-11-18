package sqlite

import (
	"code-agent/internal/session"
	"context"
	"testing"
)

func TestSharedDeliveryRollsBackBothInboxes(t *testing.T) {
	store, inbox := supplementFixture(t)
	ctx := context.Background()
	sess, err := store.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = inbox.AcceptSupplement(ctx, supplement("user")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.RecordRuntimeInput(ctx, sampleRuntimeInput("runtime"), nil); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`CREATE TRIGGER reject_runtime BEFORE INSERT ON messages WHEN NEW.runtime_input=1 BEGIN SELECT RAISE(ABORT,'rollback'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inbox.CheckpointTurnDelivery(ctx, sess, session.TurnDeliveryBoundary{TurnID: "target", Deliver: true, Finalize: true}); err == nil {
		t.Fatal("expected rollback")
	}
	if len(sess.Messages) != 1 {
		t.Fatal("memory changed", sess.Messages)
	}
	receipt, err := inbox.Supplement(ctx, "owner", "user")
	if err != nil || receipt.State != "pending_apply" {
		t.Fatal(receipt, err)
	}
	events, err := store.SessionEvents(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "user_supplement_applied" {
			t.Fatal("partial event committed")
		}
	}
	if _, err = store.db.Exec(`DROP TRIGGER reject_runtime`); err != nil {
		t.Fatal(err)
	}
	result, err := inbox.CheckpointTurnDelivery(ctx, sess, session.TurnDeliveryBoundary{TurnID: "target", Deliver: true, Finalize: true})
	if err != nil || result.Closed || len(result.Applications) != 1 || len(result.RuntimeIDs) != 1 {
		t.Fatal(result, err)
	}
	result, err = inbox.CheckpointTurnDelivery(ctx, sess, session.TurnDeliveryBoundary{TurnID: "target", Deliver: true, Finalize: true})
	if err != nil || !result.Closed {
		t.Fatal(result, err)
	}
}

func TestSharedDeliveryAckOnlyRejectsStaleSupplementContext(t *testing.T) {
	store, inbox := supplementFixture(t)
	ctx := context.Background()
	sess, err := store.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	stale := *sess
	if _, _, err = inbox.AcceptSupplement(ctx, supplement("user")); err != nil {
		t.Fatal(err)
	}
	if _, err = inbox.CheckpointTurnDelivery(ctx, sess, session.TurnDeliveryBoundary{TurnID: "target", Deliver: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = inbox.CheckpointTurnDelivery(ctx, &stale, session.TurnDeliveryBoundary{TurnID: "target", Receipts: []string{"result"}}); err != session.ErrSupplementStaleContext {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, "owner")
	if err != nil || len(loaded.Messages) != 2 {
		t.Fatal(loaded, err)
	}
}

func TestSharedDeliveryResumeCannotReviveCancelledTurn(t *testing.T) {
	store, inbox := supplementFixture(t)
	ctx := context.Background()
	if _, err := store.db.Exec(`UPDATE runtime_continuations SET turn_id='target',state='active' WHERE session_id='owner'`); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTurnInputState(ctx, "owner", "initial", session.TurnInputWaitingBackground); err != nil {
		t.Fatal(err)
	}
	if err := inbox.ResumeTurnDeliveryBoundary(ctx, "owner", "target"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTurnInputState(ctx, "owner", "initial", session.TurnInputCancelled); err != nil {
		t.Fatal(err)
	}
	if err := inbox.ResumeTurnDeliveryBoundary(ctx, "owner", "target"); err != session.ErrSupplementTargetClosed {
		t.Fatal(err)
	}
	input, err := store.TurnInput(ctx, "owner", "initial")
	if err != nil || input.State != session.TurnInputCancelled {
		t.Fatal(input, err)
	}
}
