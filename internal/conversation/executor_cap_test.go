package conversation

import (
	"context"
	"encoding/json"
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

	// The cap budgets stream bytes, not the serialized Event envelope. Assert
	// head/tail separately so adding unrelated Event fields cannot change it.
	headBytes, serializedBytes, envelopeBytes, encodedChunkBytes := 0, 0, 0, 0
	streamRecords := 0
	markers := 0
	for _, r := range events.records {
		if r.Kind == string(agent.EventToolStdout) {
			var ev agent.Event
			if err := json.Unmarshal(r.Payload, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.SessionID != "s" || ev.TurnID != "t" || ev.CallID != callID {
				t.Fatalf("stream record lost its execution identity: %+v", ev)
			}
			streamRecords++
			serializedBytes += len(r.Payload)
			encoded, err := json.Marshal(ev.Chunk)
			if err != nil {
				t.Fatal(err)
			}
			encodedChunkBytes += len(encoded) - len(`""`)
			if strings.Contains(ev.Chunk, "truncated") {
				markers++
				parts := strings.SplitN(ev.Chunk, "\n", 3)
				if len(parts) != 3 || parts[0] != "" || !strings.Contains(parts[1], "[tool output truncated:") {
					t.Fatalf("invalid truncation marker: %q", ev.Chunk)
				}
				if parts[2] != strings.Repeat("x", agent.ToolStreamTailBytes) || len(ev.Chunk)-len(parts[2]) > 1024 {
					t.Fatal("marker must retain only the bounded overflow tail and summary")
				}
			} else {
				if ev.Chunk != chunk {
					t.Fatal("persisted head chunk was changed")
				}
				headBytes += len(ev.Chunk)
			}
			ev.Chunk = ""
			envelope, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			envelopeBytes += len(envelope)
		}
	}
	if headBytes != agent.ToolStreamBudget {
		t.Fatalf("persisted head %d bytes, budget %d", headBytes, agent.ToolStreamBudget)
	}
	if markers != 1 {
		t.Fatalf("expected 1 truncation marker persisted, got %d", markers)
	}
	if streamRecords != agent.ToolStreamBudget/len(chunk)+1 {
		t.Fatalf("unexpected persisted stream record count: %d", streamRecords)
	}
	if serializedBytes != envelopeBytes+encodedChunkBytes {
		t.Fatalf("serialized bytes %d != envelope %d + encoded stream %d", serializedBytes, envelopeBytes, encodedChunkBytes)
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
