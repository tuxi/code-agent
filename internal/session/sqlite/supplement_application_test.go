package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

func TestSupplementApplicationAtomicReplayAndFinalize(t *testing.T) {
	store, inbox := supplementFixture(t)
	ctx := context.Background()
	sess, err := store.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	stale := *sess
	for _, id := range []string{"a", "b"} {
		if _, _, err = inbox.AcceptSupplement(ctx, supplement(id)); err != nil {
			t.Fatal(err)
		}
	}
	applications, err := inbox.ApplySupplements(ctx, sess, "target", true)
	if err != nil || len(applications) != 2 || applications[0].EventSequence >= applications[1].EventSequence {
		t.Fatal(applications, err)
	}
	if len(sess.Messages) != 3 || sess.Messages[1].Content != "same text" || sess.Messages[2].Content != "same text" || sess.Messages[1].RuntimeInput {
		t.Fatal(sess.Messages)
	}
	if _, err = inbox.ApplySupplements(ctx, &stale, "target", false); !errors.Is(err, session.ErrSupplementStaleContext) {
		t.Fatal("stale overwrite", err)
	}
	if err = store.reopen(); err != nil {
		t.Fatal(err)
	}
	inbox, err = NewSupplementInbox(store)
	if err != nil {
		t.Fatal(err)
	}
	sess, err = store.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	applications, err = inbox.ApplySupplements(ctx, sess, "target", false)
	if err != nil || len(applications) != 0 || len(sess.Messages) != 3 {
		t.Fatal(applications, err)
	}
	events, err := store.SessionEvents(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "user_supplement_applied" {
			count++
		}
	}
	if count != 2 {
		t.Fatal(events)
	}
	receipt, err := inbox.Supplement(ctx, "owner", "a")
	if err != nil || receipt.State != "applied" {
		t.Fatal(receipt, err)
	}
	if _, err = inbox.ApplySupplements(ctx, sess, "target", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err = inbox.AcceptSupplement(ctx, supplement("late")); !errors.Is(err, session.ErrSupplementTargetClosed) {
		t.Fatal(err)
	}
}

func TestSupplementApplicationRollbackLeavesMemoryHistoryAndReceiptsUnchanged(t *testing.T) {
	store, inbox := supplementFixture(t)
	ctx := context.Background()
	sess, err := store.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = inbox.AcceptSupplement(ctx, supplement("a")); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`CREATE TRIGGER reject_application BEFORE INSERT ON messages WHEN NEW.content='same text' BEGIN SELECT RAISE(ABORT,'test rollback'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inbox.ApplySupplements(ctx, sess, "target", false); err == nil {
		t.Fatal("expected save failure")
	}
	if len(sess.Messages) != 1 || sess.Metadata["user_supplement_receipts:target"] != nil {
		t.Fatal(sess)
	}
	saved, err := store.Load(ctx, "owner")
	if err != nil || len(saved.Messages) != 1 {
		t.Fatal(saved, err)
	}
	receipt, err := inbox.Supplement(ctx, "owner", "a")
	if err != nil || receipt.State != "pending_apply" {
		t.Fatal(receipt, err)
	}
	events, err := store.SessionEvents(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "user_supplement_applied" {
			t.Fatal("rolled back event persisted")
		}
	}
	if _, err = store.db.Exec(`DROP TRIGGER reject_application`); err != nil {
		t.Fatal(err)
	}
	applied, err := inbox.ApplySupplements(ctx, sess, "target", false)
	if err != nil || len(applied) != 1 {
		t.Fatal(applied, err)
	}
}

func TestSupplementApplicationWaitsForToolResults(t *testing.T) {
	store, inbox := supplementFixture(t)
	ctx := context.Background()
	sess, err := store.Load(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = inbox.AcceptSupplement(ctx, supplement("a")); err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call"}}})
	if _, err = inbox.ApplySupplements(ctx, sess, "target", false); !errors.Is(err, session.ErrSupplementUnsafeBoundary) {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleTool, ToolCallID: "call", Content: "result"})
	applied, err := inbox.ApplySupplements(ctx, sess, "target", false)
	if err != nil || len(applied) != 1 {
		t.Fatal(applied, err)
	}
	if sess.Messages[len(sess.Messages)-2].Role != model.RoleTool || sess.Messages[len(sess.Messages)-1].Role != model.RoleUser {
		t.Fatal(sess.Messages)
	}
}

func TestSupplementApplicationFinalizeAcceptRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		store, inbox := supplementFixture(t)
		ctx := context.Background()
		sess, err := store.Load(ctx, "owner")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var acceptErr, applyErr error
		var applied []session.SupplementApplication
		wg.Add(2)
		go func() { defer wg.Done(); _, _, acceptErr = inbox.AcceptSupplement(ctx, supplement("race")) }()
		go func() { defer wg.Done(); applied, applyErr = inbox.ApplySupplements(ctx, sess, "target", true) }()
		wg.Wait()
		if applyErr != nil {
			t.Fatal(applyErr)
		}
		if acceptErr == nil {
			// Acceptance can follow the application transaction only if it stayed open.
			if len(applied) != 1 {
				t.Fatalf("accepted input omitted at finalize: %v", applied)
			}
		} else if !errors.Is(acceptErr, session.ErrSupplementTargetClosed) || len(applied) != 0 {
			t.Fatal(acceptErr, applied)
		}
	}
}
