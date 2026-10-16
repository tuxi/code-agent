package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"code-agent/internal/session"
)

func storedRecord(seq int64, sessionID, kind, payload string) session.EventRecord {
	return session.EventRecord{
		Seq:       seq,
		SessionID: sessionID,
		Kind:      kind,
		At:        time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Second),
		Payload:   []byte(payload),
	}
}

// SessionEventsSinceBudget returns the bounded newest-first tail in ascending
// seq order and reports truncation when the payload budget is exceeded.
func TestStoreSessionEventsSinceBudget(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	sid := "s1"
	// 3 small events then one big 1MB tool_stdout blob.
	_, _ = store.RecordEvent(ctx, storedRecord(0, sid, "turn_started", `{"kind":"turn_started","text":"hi"}`))
	_, _ = store.RecordEvent(ctx, storedRecord(0, sid, "thinking", `{"kind":"thinking","text":"think"}`))
	_, _ = store.RecordEvent(ctx, storedRecord(0, sid, "tool_finished", `{"kind":"tool_finished","text":"ok"}`))
	big := strings.Repeat("x", 1024*1024)
	_, _ = store.RecordEvent(ctx, storedRecord(0, sid, "tool_stdout", `{"kind":"tool_stdout","chunk":"`+big+`"}`))

	// Budget smaller than the big blob: only the newest (big) event fits before
	// the first check — but the check runs before adding each row, so with a
	// budget of 200KB the big blob (1MB) is the first row and is always taken
	// (the guard only drops rows AFTER the first). Verify the invariant: the
	// returned tail is in ascending seq order and no more than one over budget.
	recs, truncated, err := store.SessionEventsSinceBudget(ctx, sid, 0, 200*1024)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("expected truncated=true (big blob exceeds budget)")
	}
	if len(recs) == 0 {
		t.Fatal("expected at least the newest event")
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Seq <= recs[i-1].Seq {
			t.Fatalf("seq not ascending: %+v", recs)
		}
	}
	// The tail must be the NEWEST rows: the big blob's seq is 4.
	if recs[len(recs)-1].Seq != 4 {
		t.Fatalf("newest event seq = %d, want 4 (tail must be newest-first then reversed)", recs[len(recs)-1].Seq)
	}
}

// A budget that covers everything reports no truncation and returns all rows.
func TestStoreSessionEventsSinceBudgetCoversAll(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	sid := "s1"
	for i := 0; i < 5; i++ {
		_, _ = store.RecordEvent(ctx, storedRecord(0, sid, "turn_started", `{"kind":"turn_started","text":"`+strings.Repeat("a", 100)+`"}`))
	}
	recs, truncated, err := store.SessionEventsSinceBudget(ctx, sid, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("expected no truncation when budget covers everything")
	}
	if len(recs) != 5 {
		t.Fatalf("got %d events, want 5", len(recs))
	}
	if recs[0].Seq != 1 || recs[4].Seq != 5 {
		t.Fatalf("seqs = %d..%d, want 1..5", recs[0].Seq, recs[4].Seq)
	}
}

// SessionEventsThroughTurn returns the turn-aligned prefix and reports a missing
// terminal event for unknown or still-running turns.
func TestStoreSessionEventsThroughTurn(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	sid := "s1"
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	i := 0
	add := func(turn, kind string) {
		_, _ = store.RecordEvent(ctx, session.EventRecord{
			SessionID: sid, TurnID: turn, Kind: kind,
			At: base.Add(time.Duration(i) * time.Second), Payload: []byte(`{"kind":"` + kind + `"}`),
		})
		i++
	}
	add("t1", "turn_started")
	add("t1", "tool_started")
	add("t1", "turn_finished")
	add("t2", "turn_started")
	add("t2", "turn_finished")

	recs, err := store.SessionEventsThroughTurn(ctx, sid, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[2].Kind != "turn_finished" || recs[2].TurnID != "t1" {
		t.Fatalf("prefix = %+v, want the 3 events of t1", recs)
	}

	all, err := store.SessionEventsThroughTurn(ctx, sid, "t2")
	if err != nil || len(all) != 5 {
		t.Fatalf("t2 prefix = %d events err=%v, want 5", len(all), err)
	}

	if _, err := store.SessionEventsThroughTurn(ctx, sid, "missing"); !errors.Is(err, session.ErrEventTurnNotFound) {
		t.Fatalf("missing turn error = %v, want ErrEventTurnNotFound", err)
	}
	add("t3", "turn_started") // never terminates
	if _, err := store.SessionEventsThroughTurn(ctx, sid, "t3"); !errors.Is(err, session.ErrEventTurnNotFound) {
		t.Fatalf("running turn error = %v, want ErrEventTurnNotFound", err)
	}
}

// SessionEventsByKind returns only the requested kinds in seq order.
func TestStoreSessionEventsByKind(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	sid := "s1"
	_, _ = store.RecordEvent(ctx, storedRecord(0, sid, "turn_started", `{"kind":"turn_started"}`))
	_, _ = store.RecordEvent(ctx, storedRecord(0, sid, "tool_stdout", `{"kind":"tool_stdout","chunk":"`+strings.Repeat("x", 4096)+`"}`))
	_, _ = store.RecordEvent(ctx, storedRecord(0, sid, "turn_finished", `{"kind":"turn_finished"}`))

	recs, err := store.SessionEventsByKind(ctx, sid, []string{"turn_started", "turn_finished"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d events, want 2 (only turn events)", len(recs))
	}
	if recs[0].Kind != "turn_started" || recs[1].Kind != "turn_finished" {
		t.Fatalf("kinds = %s, %s; want turn_started, turn_finished", recs[0].Kind, recs[1].Kind)
	}
	if recs[0].Seq != 1 || recs[1].Seq != 3 {
		t.Fatalf("seqs = %d, %d; want 1, 3", recs[0].Seq, recs[1].Seq)
	}

	// Empty kind list is a no-op.
	none, err := store.SessionEventsByKind(ctx, sid, nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("nil kinds: len=%d err=%v, want 0 nil", len(none), err)
	}
}
