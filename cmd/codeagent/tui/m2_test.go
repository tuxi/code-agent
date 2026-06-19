package tui

import (
	"strings"
	"testing"

	"code-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

// readyModel builds a sized model and plays the given events through Update, so
// the model state is populated as it would be live.
func readyModel(t *testing.T, events ...agent.Event) model {
	t.Helper()
	m := asModel(t, must(newTestModel().Update(tea.WindowSizeMsg{Width: 80, Height: 24})))
	for _, ev := range events {
		m = asModel(t, must(m.Update(eventMsg(ev))))
	}
	return m
}

// The reply is one rendered item with its own label (§3.1 / §7).
func TestAssistantRenders(t *testing.T) {
	out := strings.Join(renderEntry(Item{Kind: ItemAssistant, Text: "all fixed"}, 80), "\n")
	if !strings.Contains(out, "all fixed") || !strings.Contains(out, "assistant") {
		t.Fatalf("assistant render = %q", out)
	}
}
