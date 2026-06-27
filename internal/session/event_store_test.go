package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestEventStoreRoundTrip(t *testing.T) {
	store := NewMemoryStore()
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)

	events := []EventRecord{
		{SessionID: "s1", TurnID: "t1", Kind: "turn_started", At: now, Payload: json.RawMessage(`{"text":"fix it"}`)},
		{SessionID: "s1", TurnID: "t1", Kind: "tool_started", At: now.Add(time.Second), Payload: json.RawMessage(`{"tool":"grep"}`)},
		{SessionID: "s1", TurnID: "t1", Kind: "tool_finished", At: now.Add(2 * time.Second), Payload: json.RawMessage(`{"ok":true}`)},
		{SessionID: "s2", TurnID: "t9", Kind: "turn_started", At: now, Payload: json.RawMessage(`{"text":"other session"}`)},
	}
	for _, e := range events {
		if err := store.RecordEvent(ctx, e); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}

	got, err := store.SessionEvents(ctx, "s1")
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events for s1 (s2's event must not leak), got %d", len(got))
	}
	// Emission order is preserved.
	if got[0].Kind != "turn_started" || got[1].Kind != "tool_started" || got[2].Kind != "tool_finished" {
		t.Fatalf("events out of order: %v %v %v", got[0].Kind, got[1].Kind, got[2].Kind)
	}
	if got[0].TurnID != "t1" || string(got[1].Payload) != `{"tool":"grep"}` {
		t.Fatalf("fields not round-tripped: %+v", got[1])
	}
	if !got[2].At.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("timestamp not round-tripped: %v", got[2].At)
	}
}

func TestDeleteRemovesEvents(t *testing.T) {
	store := NewMemoryStore()
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.RecordEvent(ctx, EventRecord{SessionID: "doomed", Kind: "thinking", At: time.Now()}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if err := store.Delete(ctx, "doomed"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := store.SessionEvents(ctx, "doomed")
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Delete should have removed the session's events, got %d", len(got))
	}
}
