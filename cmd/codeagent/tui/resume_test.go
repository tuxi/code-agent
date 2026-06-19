package tui

import (
	"strings"
	"testing"
	"time"

	"code-agent/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func twoSessions() []session.Meta {
	return []session.Meta{
		{ID: "s1", Title: "查看 agent/loop 实现", Model: "deepseek", MessageCount: 10, UpdatedAt: time.Now().Add(-15 * time.Second)},
		{ID: "s2", Title: "TUI 工作台设计", Model: "deepseek", MessageCount: 40, UpdatedAt: time.Now().Add(-18 * time.Minute)},
	}
}

func TestResumeOpensPickerNavigatesAndSwaps(t *testing.T) {
	m := readyModel(t)
	m.src.list = twoSessions
	m.src.resume = func(id string) (*session.Session, error) { return &session.Session{ID: id}, nil }

	// /resume opens the picker.
	m.composer.SetValue("/resume")
	m = asModel(t, must(m.Update(tea.KeyMsg{Type: tea.KeyEnter})))
	if m.picker == nil || len(m.picker.metas) != 2 {
		t.Fatalf("/resume should open a picker listing 2 sessions, got %+v", m.picker)
	}

	// ↓ moves the selection.
	m = asModel(t, must(m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})))
	if m.picker.idx != 1 {
		t.Fatalf("down should select index 1, got %d", m.picker.idx)
	}

	// Enter resumes the selected session: picker closes, header updates, and the
	// session is handed to the run loop.
	m = asModel(t, must(m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})))
	if m.picker != nil {
		t.Fatal("resume should close the picker")
	}
	if m.header.Session != "s2" {
		t.Fatalf("header should switch to s2, got %q", m.header.Session)
	}
	select {
	case got := <-m.b.sessSwap:
		if got.ID != "s2" {
			t.Fatalf("run loop should receive s2, got %q", got.ID)
		}
	default:
		t.Fatal("resume should hand the new session to the run loop")
	}
}

func TestResumeEscCancels(t *testing.T) {
	m := readyModel(t)
	m.src.list = twoSessions
	m.picker = &sessionPicker{metas: twoSessions()}
	m = asModel(t, must(m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEsc})))
	if m.picker != nil {
		t.Fatal("esc should cancel the picker")
	}
}

func TestResumeRefusedMidTurn(t *testing.T) {
	m := readyModel(t)
	m.src.list = twoSessions
	m.busy = true
	cmd := m.openResume("")
	if m.picker != nil {
		t.Fatal("a turn is running — the picker should not open")
	}
	if cmd == nil {
		t.Fatal("should explain why resume is refused")
	}
}

func TestPickerRendersTitleAndCursor(t *testing.T) {
	out := strings.Join(renderPicker(sessionPicker{metas: twoSessions(), idx: 0}, 80), "\n")
	if !strings.Contains(out, "查看 agent/loop 实现") {
		t.Fatalf("picker should show the session title:\n%s", out)
	}
	if !strings.Contains(out, "❯") {
		t.Fatal("the selected session should be marked")
	}
	if !strings.Contains(out, "ago") {
		t.Fatal("each session should show a relative time")
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
