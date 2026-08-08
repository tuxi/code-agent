package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"code-agent/cmd/codeagent/tui/components/chat"
)

// appWithFold builds a real app model with one thinking fold, renders the full
// view, and returns the terminal row where the fold summary renders.
func appWithFold(t *testing.T, w, h int) (*model, int) {
	t.Helper()
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Update(chat.BatchMessagesMsg{Messages: []chat.Message{
		{ID: "u1", Kind: chat.KindUser, Content: "hello"},
		{ID: "t1", Kind: chat.KindThinking, Fold: &chat.Fold{ID: "t1", Title: "Thought", Count: 1}},
		{ID: "a1", Kind: chat.KindAssistant, Content: "done", Finished: true},
	}})
	lines := strings.Split(m.View().Content, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "▸") {
			return m, i
		}
	}
	return m, -1
}

// TestAppClickToggleAtSummaryRow is the regression test for the click offset:
// the fold summary renders at a real terminal row and a click at exactly that
// row must toggle it. Previously the split pane swallowed tea.WindowSizeMsg
// without forwarding it to the chat page, so syncScreenOrigin never ran and
// screenY stayed 0 — every click landed one row off (the container's top
// padding was mis-mapped onto the first content row).
func TestAppClickToggleAtSummaryRow(t *testing.T) {
	// Collapsed model: click at terminal row 0, the container's top padding
	// (the list starts at screenY=1), must be a no-op — it maps to a negative
	// list row.
	m, summaryRow := appWithFold(t, 80, 30)
	if summaryRow < 0 {
		t.Fatal("no fold summary rendered")
	}
	m0, _ := m.Update(tea.MouseClickMsg{X: 1, Y: 0, Button: tea.MouseLeft})
	mm0 := m0.(*model)
	if !strings.Contains(mm0.View().Content, "▸") {
		t.Fatal("click at row 0 (padding above the list) must not toggle")
	}
	// Click exactly at the rendered summary row.
	m1, _ := mm0.Update(tea.MouseClickMsg{X: 1, Y: summaryRow, Button: tea.MouseLeft})
	mm1 := m1.(*model)
	if strings.Contains(mm1.View().Content, "▸") {
		t.Fatalf("click at the summary's terminal row %d should expand the fold", summaryRow)
	}
}
