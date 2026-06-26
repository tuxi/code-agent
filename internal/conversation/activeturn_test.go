package conversation

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func TestActiveTurnRegistry_BeginFinish(t *testing.T) {
	r := NewActiveTurnRegistry()
	ctx := context.Background()

	turnCtx, cancel, err := r.BeginTurn("s1", ctx)
	if err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if turnCtx == nil {
		t.Fatal("nil context")
	}
	if cancel == nil {
		t.Fatal("nil cancel")
	}

	// Session should be active.
	r.FinishTurn("s1")

	// Idempotent finish.
	r.FinishTurn("s1")
	r.FinishTurn("nonexistent")
}

func TestActiveTurnRegistry_ErrBusy(t *testing.T) {
	r := NewActiveTurnRegistry()
	ctx := context.Background()

	_, _, err := r.BeginTurn("s1", ctx)
	if err != nil {
		t.Fatalf("first BeginTurn: %v", err)
	}

	// Second call for same session must fail.
	_, _, err = r.BeginTurn("s1", ctx)
	if err != ErrBusy {
		t.Errorf("want ErrBusy, got %v", err)
	}

	// Finish and retry — should succeed.
	r.FinishTurn("s1")
	_, _, err = r.BeginTurn("s1", ctx)
	if err != nil {
		t.Errorf("after Finish: %v", err)
	}
	r.FinishTurn("s1")
}

func TestActiveTurnRegistry_Cancel(t *testing.T) {
	r := NewActiveTurnRegistry()
	ctx := context.Background()

	turnCtx, _, _ := r.BeginTurn("s1", ctx)

	// Cancel the turn.
	r.Cancel("s1")

	// The derived context should be canceled.
	select {
	case <-turnCtx.Done():
		// ok
	default:
		t.Error("context not canceled after Cancel")
	}

	// Cancel is no-op when idle.
	r.FinishTurn("s1")
	r.Cancel("s1") // shouldn't panic
	r.Cancel("nonexistent")
}

func TestActiveTurnRegistry_Approver(t *testing.T) {
	r := NewActiveTurnRegistry()

	if a := r.Approver("s1"); a != nil {
		t.Error("unknown session should return nil approver")
	}

	fa := &fakeApprover{allow: true}
	r.SetApprover("s1", fa)
	if a := r.Approver("s1"); a != fa {
		t.Error("approver not set")
	}

	// Clear approver.
	r.SetApprover("s1", nil)
	if a := r.Approver("s1"); a != nil {
		t.Error("approver should be nil after clearing")
	}

	// Clearing with active turn should not clean up (cancel is non-nil).
	_, _, _ = r.BeginTurn("s1", context.Background())
	r.SetApprover("s1", fa)
	r.SetApprover("s1", nil) // still has active turn
	r.Cancel("s1")
	r.FinishTurn("s1") // now both nil → cleanup
}

func TestActiveTurnRegistry_Shutdown(t *testing.T) {
	r := NewActiveTurnRegistry()
	ctx := context.Background()

	turnCtx, _, _ := r.BeginTurn("s1", ctx)

	r.Shutdown()

	select {
	case <-turnCtx.Done():
		// ok — Shutdown cancels active turns
	default:
		t.Error("Shutdown did not cancel active turn")
	}

	// Post-shutdown operations should fail.
	_, _, err := r.BeginTurn("s2", ctx)
	if err == nil {
		t.Error("BeginTurn should fail after Shutdown")
	}
	// SetApprover should be no-op.
	r.SetApprover("s2", &fakeApprover{true})
	if a := r.Approver("s2"); a != nil {
		t.Error("SetApprover should be no-op after Shutdown")
	}
}

func TestActiveTurnRegistry_Concurrent(t *testing.T) {
	r := NewActiveTurnRegistry()
	ctx := context.Background()

	const n = 20
	errs := make(chan error, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := r.BeginTurn("s1", ctx)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	successes := 0
	busy := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if err == ErrBusy {
			busy++
		}
	}
	if successes != 1 {
		t.Errorf("want exactly 1 success, got %d", successes)
	}
	if busy != n-1 {
		t.Errorf("want %d ErrBusy, got %d", n-1, busy)
	}
	r.FinishTurn("s1")
}

// fakeApprover implements agent.Approver for testing.
type fakeApprover struct{ allow bool }

func (f *fakeApprover) Approve(string, json.RawMessage) bool { return f.allow }
