package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// MemoryStore mirrors the sqlite EventBudgetStore / EventKindStore contracts.
func TestMemoryStoreSessionEventsSinceBudget(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	sid := "s1"
	_, _ = store.RecordEvent(ctx, EventRecord{SessionID: sid, Kind: "turn_started", At: time.Now(), Payload: []byte(`{"kind":"turn_started"}`)})
	_, _ = store.RecordEvent(ctx, EventRecord{SessionID: sid, Kind: "tool_finished", At: time.Now(), Payload: []byte(`{"kind":"tool_finished"}`)})
	big := strings.Repeat("x", 4096)
	_, _ = store.RecordEvent(ctx, EventRecord{SessionID: sid, Kind: "tool_stdout", At: time.Now(), Payload: []byte(`{"chunk":"` + big + `"}`)})

	recs, truncated, err := store.SessionEventsSinceBudget(ctx, sid, 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if len(recs) != 1 {
		t.Fatalf("got %d events, want 1 (only the newest fits before the guard)", len(recs))
	}
	if recs[0].Kind != "tool_stdout" {
		t.Fatalf("newest event kind = %s, want tool_stdout", recs[0].Kind)
	}
	if recs[0].Seq != 3 {
		t.Fatalf("newest seq = %d, want 3", recs[0].Seq)
	}
}

func TestMemoryStoreSessionEventsSinceBudgetCoversAll(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	sid := "s1"
	for i := 0; i < 5; i++ {
		_, _ = store.RecordEvent(ctx, EventRecord{SessionID: sid, Kind: "thinking", At: time.Now(), Payload: []byte(`{"k":"v"}`)})
	}
	recs, truncated, err := store.SessionEventsSinceBudget(ctx, sid, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("expected no truncation when budget covers everything")
	}
	if len(recs) != 5 || recs[0].Seq != 1 || recs[4].Seq != 5 {
		t.Fatalf("got %d events seq %d..%d, want 5 events 1..5", len(recs), recs[0].Seq, recs[4].Seq)
	}
}

// SessionEventsThroughTurn returns the turn-aligned prefix and reports a missing
// terminal event for unknown or still-running turns.
func TestMemoryStoreSessionEventsThroughTurn(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	sid := "s1"
	add := func(turn, kind string) {
		_, _ = store.RecordEvent(ctx, EventRecord{SessionID: sid, TurnID: turn, Kind: kind, At: time.Now()})
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

	if _, err := store.SessionEventsThroughTurn(ctx, sid, "missing"); !errors.Is(err, ErrEventTurnNotFound) {
		t.Fatalf("missing turn error = %v, want ErrEventTurnNotFound", err)
	}
	add("t3", "turn_started") // never terminates
	if _, err := store.SessionEventsThroughTurn(ctx, sid, "t3"); !errors.Is(err, ErrEventTurnNotFound) {
		t.Fatalf("running turn error = %v, want ErrEventTurnNotFound", err)
	}
}

func TestMemoryStoreSessionEventsByKind(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	sid := "s1"
	_, _ = store.RecordEvent(ctx, EventRecord{SessionID: sid, Kind: "turn_started", At: time.Now()})
	_, _ = store.RecordEvent(ctx, EventRecord{SessionID: sid, Kind: "tool_stdout", At: time.Now(), Payload: []byte(strings.Repeat("x", 4096))})
	_, _ = store.RecordEvent(ctx, EventRecord{SessionID: sid, Kind: "turn_finished", At: time.Now()})

	recs, err := store.SessionEventsByKind(ctx, sid, []string{"turn_started", "turn_finished"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Kind != "turn_started" || recs[1].Kind != "turn_finished" {
		t.Fatalf("got %+v, want only the two turn events", recs)
	}
	if recs[0].Seq != 1 || recs[1].Seq != 3 {
		t.Fatalf("seqs = %d, %d; want 1, 3", recs[0].Seq, recs[1].Seq)
	}
}
