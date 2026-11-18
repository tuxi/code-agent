package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"code-agent/internal/session"
)

func supplementFixture(t *testing.T) (*Store, *SupplementInbox) {
	t.Helper()
	store, sess := runtimeInputFixture(t)
	ctx := context.Background()
	input, _, _, err := store.ReserveTurnInput(ctx, session.TurnInput{SessionID: sess.ID, RequestID: "initial", TurnID: "target", PayloadHash: "initial", Text: "task"}, session.EventRecord{SessionID: sess.ID, TurnID: "target", Kind: "turn_accepted", At: time.Now(), Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StartTurnInput(ctx, input, sess); err != nil {
		t.Fatal(err)
	}
	inbox, err := NewSupplementInbox(store)
	if err != nil {
		t.Fatal(err)
	}
	if err = inbox.OpenSupplementBoundary(ctx, sess.ID, "target"); err != nil {
		t.Fatal(err)
	}
	return store, inbox
}

func supplement(id string) session.Supplement {
	return session.Supplement{SessionID: "owner", RequestID: id, ExpectedTurnID: "target", Text: "same text"}
}

func TestSupplementIdentityOrderAndReopen(t *testing.T) {
	store, inbox := supplementFixture(t)
	ctx := context.Background()
	if err := inbox.OpenSupplementBoundary(ctx, "owner", "target"); err != nil {
		t.Fatal(err)
	}
	first, created, err := inbox.AcceptSupplement(ctx, supplement("a"))
	if err != nil || !created {
		t.Fatal(first, created, err)
	}
	duplicate, created, err := inbox.AcceptSupplement(ctx, supplement("a"))
	if err != nil || created || duplicate.Sequence != first.Sequence {
		t.Fatal(duplicate, created, err)
	}
	second, created, err := inbox.AcceptSupplement(ctx, supplement("b"))
	if err != nil || !created || second.Sequence <= first.Sequence {
		t.Fatal(second, created, err)
	}
	changed := supplement("a")
	changed.Text = "different"
	if _, _, err = inbox.AcceptSupplement(ctx, changed); !errors.Is(err, session.ErrSupplementConflict) {
		t.Fatal(err)
	}
	changed = supplement("a")
	changed.ExpectedTurnID = "another"
	if _, _, err = inbox.AcceptSupplement(ctx, changed); !errors.Is(err, session.ErrSupplementConflict) {
		t.Fatal(err)
	}
	if err = store.reopen(); err != nil {
		t.Fatal(err)
	}
	inbox, err = NewSupplementInbox(store)
	if err != nil {
		t.Fatal(err)
	}
	items, err := inbox.PendingSupplements(ctx, "owner", "target")
	if err != nil || len(items) != 2 || items[0].Sequence != first.Sequence || items[1].Sequence != second.Sequence {
		t.Fatal(items, err)
	}
	receipt, err := inbox.Supplement(ctx, "owner", "a")
	if err != nil || receipt.State != "pending_apply" {
		t.Fatal(receipt, err)
	}
	if err = inbox.SealSupplementBoundary(ctx, "owner", "target"); !errors.Is(err, session.ErrSupplementPending) {
		t.Fatal(err)
	}
	// A failed seal rolls back rather than silently losing later supplements.
	if _, created, err = inbox.AcceptSupplement(ctx, supplement("c")); err != nil || !created {
		t.Fatal(created, err)
	}
}

func TestSupplementSealAcceptRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		_, inbox := supplementFixture(t)
		ctx := context.Background()
		var wg sync.WaitGroup
		var acceptErr, sealErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, _, acceptErr = inbox.AcceptSupplement(ctx, supplement("race")) }()
		go func() { defer wg.Done(); sealErr = inbox.SealSupplementBoundary(ctx, "owner", "target") }()
		wg.Wait()
		switch {
		case acceptErr == nil:
			if !errors.Is(sealErr, session.ErrSupplementPending) {
				t.Fatalf("accepted input lost at close: %v", sealErr)
			}
		case errors.Is(acceptErr, session.ErrSupplementTargetClosed):
			if sealErr != nil {
				t.Fatalf("closed target: %v", sealErr)
			}
			if err := inbox.OpenSupplementBoundary(ctx, "owner", "target"); !errors.Is(err, session.ErrSupplementTargetClosed) {
				t.Fatalf("sealed target reopened: %v", err)
			}
		default:
			t.Fatalf("unexpected race: %v / %v", acceptErr, sealErr)
		}
	}
}

func TestSupplementConcurrentDuplicateAndClosedTarget(t *testing.T) {
	_, inbox := supplementFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := inbox.AcceptSupplement(ctx, supplement("duplicate")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	items, err := inbox.PendingSupplements(ctx, "owner", "target")
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	wrong := supplement("wrong")
	wrong.ExpectedTurnID = "next-turn"
	if _, _, err = inbox.AcceptSupplement(ctx, wrong); !errors.Is(err, session.ErrSupplementTargetClosed) {
		t.Fatal(err)
	}
	empty := supplement("empty")
	empty.Text = " "
	if _, _, err = inbox.AcceptSupplement(ctx, empty); !errors.Is(err, session.ErrInvalidSupplement) {
		t.Fatal(err)
	}
}
