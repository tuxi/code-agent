package tui

import (
	"strings"
	"testing"

	"code-agent/internal/agent"
)

func heartbeatModel() model {
	return newModel(NewBackend(), HeaderInfo{Session: "parent", SubagentBudget: 20}, sessionSource{})
}

func TestSubagentEventsDriveHeartbeatNotThinking(t *testing.T) {
	m := heartbeatModel()

	// A sub-session event (different SessionID) activates the heartbeat.
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventTaskStarted, SessionID: "sub-1"}))))
	if !m.subActive {
		t.Fatal("task_started from a sub-session should activate the heartbeat")
	}

	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelStarted, SessionID: "sub-1"}))))
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventToolStarted, SessionID: "sub-1", ToolName: "read_file"}))))
	if m.subStep != 1 || m.subTool != "read_file" {
		t.Fatalf("heartbeat should track iteration/tool, got step=%d tool=%q", m.subStep, m.subTool)
	}
	// A sub-session model call must NOT toggle the parent's thinking spinner.
	if m.thinking {
		t.Fatal("a sub-session model call must not toggle the parent thinking state")
	}

	if sl := m.statusLine(); !strings.Contains(sl, "subagent") || !strings.Contains(sl, "step 1/20") || !strings.Contains(sl, "read_file") {
		t.Fatalf("status line should show the heartbeat, got: %q", sl)
	}

	// task_finished clears it.
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventTaskFinished, SessionID: "sub-1"}))))
	if m.subActive {
		t.Fatal("task_finished should clear the heartbeat")
	}
}

func TestParentEventsBypassHeartbeat(t *testing.T) {
	// An event carrying the parent's own session id is handled normally — it
	// toggles thinking and never touches the subagent heartbeat.
	m := heartbeatModel()
	m = asModel(t, must(m.Update(eventMsg(agent.Event{Kind: agent.EventModelStarted, SessionID: "parent"}))))
	if !m.thinking || m.subActive {
		t.Fatalf("parent event should toggle thinking, not the heartbeat: thinking=%v subActive=%v", m.thinking, m.subActive)
	}
}
