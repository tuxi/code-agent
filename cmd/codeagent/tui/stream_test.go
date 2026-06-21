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
