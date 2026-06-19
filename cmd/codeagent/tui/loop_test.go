package tui

import (
	"testing"
	"time"

	"code-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

// runLeaves runs cmd, flattening tea.BatchMsg, and returns every leaf message.
// Each leaf runs with a timeout so a listener blocked on an empty channel records
// nil instead of hanging the test — so callers must pre-queue the channel data a
// re-issued listener should pick up.
func runLeaves(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-got:
	case <-time.After(time.Second):
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runLeaves(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func hasEvent(msgs []tea.Msg, kind agent.EventKind) bool {
	for _, msg := range msgs {
		if em, ok := msg.(eventMsg); ok && agent.Event(em).Kind == kind {
			return true
		}
	}
	return false
}

// The regression this whole saga was about: after rendering an event, the model
// MUST re-issue the event listener, or the transcript goes silent after the first
// event (the symptom: only "› 你好" shows, nothing after). A tea.Sequentially that
// short-circuits, or returning only a print command, breaks this.
func TestEventListenerKeepsFiring(t *testing.T) {
	b := NewBackend()
	m := newModel(b, HeaderInfo{}, sessionSource{})
	m.width, m.ready = 80, true

	_, cmd := m.Update(eventMsg(agent.Event{Kind: agent.EventTurnStarted, Text: "hi"}))

	// Pre-queue the next event so the re-issued listener returns it immediately.
	b.events <- agent.Event{Kind: agent.EventTurnFinished, Text: "done"}

	if !hasEvent(runLeaves(cmd), agent.EventTurnFinished) {
		t.Fatal("handleEvent did not re-issue the event listener — events stop after the first one")
	}
}

// An event that renders nothing (e.g. EventModelStarted) must still re-issue the
// listener — otherwise the stream dies mid-turn on a silent event.
func TestSilentEventStillReissuesListener(t *testing.T) {
	b := NewBackend()
	m := newModel(b, HeaderInfo{}, sessionSource{})
	m.width, m.ready = 80, true

	_, cmd := m.Update(eventMsg(agent.Event{Kind: agent.EventModelStarted}))
	b.events <- agent.Event{Kind: agent.EventTurnFinished, Text: "done"}

	if !hasEvent(runLeaves(cmd), agent.EventTurnFinished) {
		t.Fatal("a silent event must still re-issue the listener")
	}
}

func TestDoneListenerKeepsFiring(t *testing.T) {
	b := NewBackend()
	m := newModel(b, HeaderInfo{}, sessionSource{})
	m.width, m.ready, m.busy = 80, true, true

	_, cmd := m.Update(doneMsg{})
	b.done <- nil

	found := false
	for _, msg := range runLeaves(cmd) {
		if _, ok := msg.(doneMsg); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("doneMsg did not re-issue the done listener — turn completion stops being detected")
	}
}

func TestApprovalListenerKeepsFiring(t *testing.T) {
	b := NewBackend()
	m := newModel(b, HeaderInfo{}, sessionSource{})
	m.width, m.ready = 80, true

	_, cmd := m.Update(approvalMsg{tool: "edit_file", reply: make(chan bool, 1)})
	// b.approvals is unbuffered (the runner blocks for an answer), so send async —
	// the re-issued waitForApproval inside cmd is the receiver.
	go func() { b.approvals <- approvalReq{tool: "create_file", reply: make(chan bool, 1)} }()

	found := false
	for _, msg := range runLeaves(cmd) {
		if _, ok := msg.(approvalMsg); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("approvalMsg did not re-issue the approval listener — later approvals would hang the runner")
	}
}
