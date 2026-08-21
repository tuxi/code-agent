package session

import (
	"context"
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
