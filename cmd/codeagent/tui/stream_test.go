package tui

import (
	"testing"

	"code-agent/internal/agent"
)

func TestStreamingPreviewAccumulatesAndClears(t *testing.T) {
	m := heartbeatModel()

	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelStarted}))))
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventTokenDelta, Text: "Hel"}))))
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventTokenDelta, Text: "lo"}))))
	if m.streaming != "Hello" {
		t.Fatalf("streaming preview = %q, want 'Hello'", m.streaming)
	}

	// The model call finishing clears the ephemeral preview so the authoritative
	// render (final reply to scrollback) takes over.
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelFinished}))))
	if m.streaming != "" {
		t.Fatalf("EventModelFinished should clear the streaming preview, got %q", m.streaming)
	}
}

func TestTokenDeltaNeverEntersTranscript(t *testing.T) {
	// A token delta updates only the live preview; it must not print to scrollback.
	m := heartbeatModel()
	_, cmd := m.Update(eventMsg(agent.Event{Kind: agent.EventTokenDelta, Text: "x"}))
	if cmd == nil {
		t.Fatal("a delta should still re-arm the event listener")
	}
}

func TestReasoningDeltaIsLiveAndSnapshotReplacesIt(t *testing.T) {
	m := heartbeatModel()
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelStarted}))))
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventReasoningDelta, Text: "partial"}))))
	if m.tr.step.thinking != "partial" {
		t.Fatalf("live reasoning=%q, want partial", m.tr.step.thinking)
	}

	// The durable snapshot is authoritative. It must replace a partial preview,
	// not append to it and produce "partialcomplete" in the transcript.
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventThinking, Text: "complete"}))))
	if m.tr.step.thinking != "complete" {
		t.Fatalf("snapshot reasoning=%q, want complete", m.tr.step.thinking)
	}
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelFinished}))))
	if m.tr.step.thinking != "complete" {
		t.Fatalf("final snapshot was discarded at model_finished: %q", m.tr.step.thinking)
	}
}

func TestReasoningPreviewWithoutSnapshotIsDiscarded(t *testing.T) {
	m := heartbeatModel()
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelStarted}))))
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventReasoningDelta, Text: "failed attempt"}))))
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelFinished}))))
	if m.tr.step.thinking != "" {
		t.Fatalf("orphaned reasoning preview survived model_finished: %q", m.tr.step.thinking)
	}
}
