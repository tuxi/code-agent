package runtime

import (
	"context"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

func TestEventStoreEmitterSkipsReasoningDeltaButPersistsSnapshot(t *testing.T) {
	store := session.NewMemoryStore()
	emitter := EventStoreEmitter{Ctx: context.Background(), Store: store}

	emitter.Emit(agent.Event{Kind: agent.EventReasoningDelta, SessionID: "s", Text: "partial"})
	emitter.Emit(agent.Event{Kind: agent.EventThinking, SessionID: "s", Text: "complete"})

	events, err := store.SessionEvents(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != string(agent.EventThinking) {
		t.Fatalf("persisted events=%+v, want only thinking snapshot", events)
	}
}
