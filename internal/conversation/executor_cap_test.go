package conversation

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

// sequencingEmitter caps per-call tool stream persistence: overflow chunks go
// live but not to the store, and a head+tail marker is persisted at
// tool_finished (persisted, not re-fanned live).
func TestSequencingEmitterCapsToolStreamPersistence(t *testing.T) {
	events := &fakeEventStore{}
	var live []agent.Event
	se := &sequencingEmitter{
		ctx:         context.Background(),
		events:      events,
		live:        emitterFunc(func(e agent.Event) { live = append(live, e) }),
		toolStreams: agent.NewToolStreamCapper(),
	}

	callID := "call_1"
	chunk := strings.Repeat("x", 8192)
	total := 0
	// Feed 3x the budget.
	for total < 3*agent.ToolStreamBudget {
		se.Emit(agent.Event{Kind: agent.EventToolStdout, SessionID: "s", TurnID: "t", CallID: callID, Chunk: chunk})
		total += len(chunk)
	}
	se.Emit(agent.Event{Kind: agent.EventToolFinished, SessionID: "s", TurnID: "t", CallID: callID, ToolName: "run_command"})

	// Persisted: bounded. Sum of persisted chunk payloads ≤ budget + one chunk
	// (the first over-budget chunk is still persisted in the head check) +
	// marker.
	persisted := int64(0)
	markers := 0
	for _, r := range events.records {
		if r.Kind == string(agent.EventToolStdout) {
			persisted += int64(len(r.Payload))
		}
		if strings.Contains(string(r.Payload), "truncated") {
			markers++
		}
	}
	if persisted > agent.ToolStreamBudget+int64(len(chunk))+4096 {
		t.Fatalf("persisted %d bytes, budget %d", persisted, agent.ToolStreamBudget)
	}
	if markers != 1 {
		t.Fatalf("expected 1 truncation marker persisted, got %d", markers)
	}

	// Live: received every chunk (nothing truncated live) and the finished
	// event, but NOT the marker (it would duplicate the tail).
	liveChunks := 0
	liveFinished := 0
	for _, e := range live {
		switch e.Kind {
		case agent.EventToolStdout:
			liveChunks++
		case agent.EventToolFinished:
			liveFinished++
		}
		if strings.Contains(e.Chunk, "truncated") {
			t.Fatalf("marker must not be fanned live: %+v", e)
		}
	}
	if liveChunks != total/len(chunk) {
		t.Fatalf("live chunks = %d, want %d (all chunks live)", liveChunks, total/len(chunk))
	}
	if liveFinished != 1 {
		t.Fatalf("live tool_finished = %d, want 1", liveFinished)
	}
}

// A tool that never emits tool_finished still gets a bounded marker at turn end.
func TestSequencingEmitterFlushesAtTurnEnd(t *testing.T) {
	events := &fakeEventStore{}
	se := &sequencingEmitter{
		ctx:         context.Background(),
		events:      events,
		live:        emitterFunc(func(agent.Event) {}),
		toolStreams: agent.NewToolStreamCapper(),
	}
	callID := "call_zombie"
	chunk := strings.Repeat("z", 4096)
	for i := 0; i < 3*agent.ToolStreamBudget/4096; i++ {
		se.Emit(agent.Event{Kind: agent.EventToolStdout, SessionID: "s", TurnID: "t", CallID: callID, Chunk: chunk})
	}
	se.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: "s", TurnID: "t", Text: "done"})

	markers := 0
	for _, r := range events.records {
		if strings.Contains(string(r.Payload), "truncated") {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("expected 1 marker from turn-end flush, got %d", markers)
	}
}

// Under-budget tool output is persisted verbatim with no marker.
func TestSequencingEmitterUnderBudgetNoMarker(t *testing.T) {
	events := &fakeEventStore{}
	var live []agent.Event
	se := &sequencingEmitter{
		ctx:         context.Background(),
		events:      events,
		live:        emitterFunc(func(e agent.Event) { live = append(live, e) }),
		toolStreams: agent.NewToolStreamCapper(),
	}
	for i := 0; i < 4; i++ {
		se.Emit(agent.Event{Kind: agent.EventToolStdout, SessionID: "s", TurnID: "t", CallID: "c", Chunk: "small chunk\n"})
	}
	se.Emit(agent.Event{Kind: agent.EventToolFinished, SessionID: "s", TurnID: "t", CallID: "c"})

	for _, r := range events.records {
		if strings.Contains(string(r.Payload), "truncated") {
			t.Fatalf("unexpected marker for under-budget call: %s", r.Payload)
		}
	}
	if len(events.records) != 5 { // 4 chunks + finished
		t.Fatalf("persisted %d records, want 5", len(events.records))
	}
	if len(live) != 5 {
		t.Fatalf("live %d events, want 5", len(live))
	}
	_ = session.EventRecord{}
}
