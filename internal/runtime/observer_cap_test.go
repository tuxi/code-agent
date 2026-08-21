package runtime

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

// EventStoreEmitter with a ToolStreamCapper bounds per-call tool stream
// persistence: overflow chunks still reach the renderer (Next), but only the
// head + a tail marker land in the store.
func TestEventStoreEmitterCapsToolStreamPersistence(t *testing.T) {
	store := session.NewMemoryStore()
	var next []agent.Event
	emitter := EventStoreEmitter{
		Ctx:         context.Background(),
		Store:       store,
		Next:        emitterFunc(func(e agent.Event) { next = append(next, e) }),
		ToolStreams: agent.NewToolStreamCapper(),
	}

	callID := "call_1"
	chunk := strings.Repeat("y", 8192)
	total := 0
	for total < 3*agent.ToolStreamBudget {
		emitter.Emit(agent.Event{Kind: agent.EventToolStdout, SessionID: "s", TurnID: "t", CallID: callID, Chunk: chunk})
		total += len(chunk)
	}
	emitter.Emit(agent.Event{Kind: agent.EventToolFinished, SessionID: "s", TurnID: "t", CallID: callID, ToolName: "run_command"})

	// Renderer got every chunk plus finished — nothing truncated live.
	if len(next) != total/len(chunk)+1 {
		t.Fatalf("renderer events = %d, want %d (all chunks + finished)", len(next), total/len(chunk)+1)
	}
	for _, e := range next {
		if strings.Contains(e.Chunk, "truncated") {
			t.Fatalf("marker must not reach the renderer: %+v", e)
		}
	}

	// Store is bounded: head + one marker, no overflow chunks.
	records, err := store.SessionEvents(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	persisted := int64(0)
	markers := 0
	for _, r := range records {
		persisted += int64(len(r.Payload))
		if strings.Contains(string(r.Payload), "truncated") {
			markers++
		}
	}
	if persisted > agent.ToolStreamBudget+int64(len(chunk))+4096 {
		t.Fatalf("persisted %d bytes, budget %d", persisted, agent.ToolStreamBudget)
	}
	if markers != 1 {
		t.Fatalf("expected 1 marker, got %d", markers)
	}
}

// WithEventStore attaches the capper by default.
func TestWithEventStoreAttachesCapper(t *testing.T) {
	store := session.NewMemoryStore()
	var next []agent.Event
	emitter := WithEventStore(emitterFunc(func(e agent.Event) { next = append(next, e) }), store, context.Background())
	ese, ok := emitter.(EventStoreEmitter)
	if !ok {
		t.Fatalf("WithEventStore returned %T, want EventStoreEmitter", emitter)
	}
	if ese.ToolStreams == nil {
		t.Fatal("WithEventStore should attach a ToolStreamCapper")
	}
}

type emitterFunc func(agent.Event)

func (f emitterFunc) Emit(e agent.Event) { f(e) }
