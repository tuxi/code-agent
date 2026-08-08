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
	m, summaryRow := appWithFold(t, 80, 30)
	if summaryRow < 0 {
		t.Fatal("no fold summary rendered")
	}
	click := func(x, y int) *model {
		m2, _ := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		m = m2.(*model)
		m3, _ := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
		return m3.(*model)
	}
	// Click at terminal row 0 (container padding above the list) is a no-op —
	// it maps to a negative list row.
	if mm := click(1, 0); !strings.Contains(mm.View().Content, "▸") {
		t.Fatal("click at row 0 (padding above the list) must not toggle")
	}
	// Click exactly at the rendered summary row.
	if mm := click(1, summaryRow); strings.Contains(mm.View().Content, "▸") {
		t.Fatalf("click at the summary's terminal row %d should expand the fold", summaryRow)
	}
}
