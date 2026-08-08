package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"code-agent/cmd/codeagent/tui/components/dialog"
	"code-agent/internal/agent"
	"code-agent/internal/session"
)

func twoSessions() []session.Meta {
	return []session.Meta{
		{ID: "s1", Title: "查看 agent/loop 实现", Model: "deepseek", MessageCount: 10, UpdatedAt: time.Now().Add(-15 * time.Second)},
		{ID: "s2", Title: "TUI 工作台设计", Model: "deepseek", MessageCount: 40, UpdatedAt: time.Now().Add(-18 * time.Minute)},
	}
}

// /resume routes through the session dialog (ctrl+s): navigation selects, enter
// emits SessionSelectedMsg, and the app resolves it through m.resume, which
// hands the session to the run loop and rebuilds the conversation from the
// session's persisted events.
func TestResumeViaSessionDialogSwaps(t *testing.T) {
	m := readyModel(t)
	m.src.list = twoSessions
	m.src.resume = func(id string) (*session.Session, error) { return &session.Session{ID: id}, nil }
	m.src.events = func(id string) []agent.Event { return nil }

	// ctrl+s opens the session dialog.
	tm, _ := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = asModel(t, tm)
	if m.dialog == nil {
		t.Fatal("ctrl+s should open the session dialog")
	}
	if _, ok := m.dialog.(dialog.SessionDialog); !ok {
		t.Fatalf("ctrl+s should open the session dialog, got %T", m.dialog)
	}

	// ↓ moves the selection to the second session; enter selects it.
	tm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = asModel(t, tm)
	tm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = asModel(t, tm)
	sel, ok := cmd().(dialog.SessionSelectedMsg)
	if !ok {
		t.Fatalf("enter on the session dialog should emit SessionSelectedMsg, got %T", cmd())
	}
	if sel.ID != "s2" {
		t.Fatalf("after ↓ the selection should be s2, got %q", sel.ID)
	}

	// The app dispatches the selection through /resume.
	tm, cmd = m.Update(sel)
	m = asModel(t, tm)
	if m.dialog != nil {
		t.Fatal("selecting a session should close the dialog")
	}
	runLeaves(cmd) // resume() hands the session to the run loop
	select {
	case got := <-m.b.sessSwap:
		if got.ID != "s2" {
			t.Fatalf("run loop should receive s2, got %q", got.ID)
		}
	default:
		t.Fatal("resume should hand the new session to the run loop")
	}
}

// esc closes the non-blocking session dialog without switching.
func TestResumeDialogEscCancels(t *testing.T) {
	m := readyModel(t)
	m.src.list = twoSessions
	tm, _ := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = asModel(t, tm)
	if m.dialog == nil {
		t.Fatal("ctrl+s should open the session dialog")
	}
	tm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = asModel(t, tm)
	if m.dialog != nil {
		t.Fatal("esc should close the session dialog")
	}
}

func TestHumanAgo(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30 seconds ago"},
		{1 * time.Minute, "1 minute ago"},
		{18 * time.Minute, "18 minutes ago"},
		{3 * time.Hour, "3 hours ago"},
	}
	for _, c := range cases {
		if got := humanAgo(time.Now().Add(-c.d)); got != c.want {
			t.Errorf("humanAgo(-%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
