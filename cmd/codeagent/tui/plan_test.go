package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPlanKeyTogglesAndShowsBadge(t *testing.T) {
	m := heartbeatModel()
	if m.planMode {
		t.Fatal("plan mode should start off")
	}

	m = asModel(t, must(m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})))
	if !m.planMode {
		t.Fatal("ctrl+p should toggle plan mode on")
	}
	if !strings.Contains(m.statusLine(), "PLAN") {
		t.Fatalf("status line should show the PLAN badge, got: %q", m.statusLine())
	}

	m = asModel(t, must(m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})))
	if m.planMode {
		t.Fatal("ctrl+p again should toggle plan mode off")
	}
}
