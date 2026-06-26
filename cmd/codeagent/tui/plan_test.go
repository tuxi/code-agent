package tui

import (
	"strings"
	"testing"

	"code-agent/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPlanKeyTogglesAndShowsBadge(t *testing.T) {
	m := heartbeatModel()
	if m.planState != agent.PlanStatusNone {
		t.Fatal("plan mode should start off")
	}

	m = asModel(t, must(m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})))
	if m.planState != agent.PlanStatusPlanning {
		t.Fatalf("ctrl+p should toggle plan mode on, got state %v", m.planState)
	}
	if !strings.Contains(m.statusLine(), "PLAN") {
		t.Fatalf("status line should show the PLAN badge, got: %q", m.statusLine())
	}

	m = asModel(t, must(m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})))
	if m.planState != agent.PlanStatusNone {
		t.Fatal("ctrl+p again should toggle plan mode off")
	}
}
