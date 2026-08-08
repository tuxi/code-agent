package chat

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The transcript viewport scrolls on mouse wheel events (the root view sets
// tea.MouseModeCellMotion). The bubbles viewport handles tea.MouseWheelMsg
// natively with a 3-line delta.
func TestListMouseWheelScrolls(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)

	// Overflow the viewport with 30 short messages.
	for i := 0; i < 30; i++ {
		msg := Message{ID: fmt.Sprintf("m%d", i), Kind: KindUser, Content: fmt.Sprintf("line %d", i)}
		m.Update(NewMessageMsg{Message: msg})
	}

	if m.viewport.TotalLineCount() <= m.viewport.Height() {
		t.Fatalf("test setup: need content taller than the viewport, total=%d height=%d",
			m.viewport.TotalLineCount(), m.viewport.Height())
	}
	bottom := m.viewport.YOffset()
	if bottom <= 0 {
		t.Fatalf("test setup: after GotoBottom the offset should be > 0, got %d", bottom)
	}

	// Wheel up scrolls toward the top.
	u, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	m = u.(*List)
	if m.viewport.YOffset() >= bottom {
		t.Fatalf("wheel up should reduce YOffset from %d, got %d", bottom, m.viewport.YOffset())
	}
	scrolled := m.viewport.YOffset()

	// Wheel down returns toward the bottom.
	u, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	m = u.(*List)
	if m.viewport.YOffset() <= scrolled {
		t.Fatalf("wheel down should increase YOffset from %d, got %d", scrolled, m.viewport.YOffset())
	}

	// Repeated wheel-up clamps at the top, never negative.
	for i := 0; i < 100; i++ {
		u, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		m = u.(*List)
	}
	if m.viewport.YOffset() != 0 {
		t.Fatalf("wheel-up to the top should clamp YOffset at 0, got %d", m.viewport.YOffset())
	}
}

// --- folding ---

// foldMessage builds a fold-representative message of the given kind.
func foldMessage(id string, kind Kind, f *Fold) Message {
	f.ID = id
	return Message{ID: id, Kind: kind, Fold: f}
}

func TestFoldToggleByEnter(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	m.SetAllowUpDown(true)
	msg := foldMessage("think1", KindThinking, &Fold{Title: "Thought", Count: 1})
	m.Update(NewMessageMsg{Message: msg})

	if m.isOpen(msg.Fold) {
		t.Fatal("a new thinking fold should default to collapsed")
	}
	// Enter without focus is a no-op.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.isOpen(msg.Fold) {
		t.Fatal("enter without a focused row must not toggle")
	}
	// Focus the fold, then Enter toggles it open.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.focusID != "think1" {
		t.Fatalf("down should focus the fold row, got %q", m.focusID)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.isOpen(msg.Fold) {
		t.Fatal("enter on a focused row should expand the fold")
	}
	// Enter again collapses it.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.isOpen(msg.Fold) {
		t.Fatal("enter on a focused expanded row should collapse the fold")
	}
	// With the composer non-empty (allowUpDown false), enter must not toggle.
	m.SetAllowUpDown(false)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.isOpen(msg.Fold) {
		t.Fatal("enter must not toggle while the composer has text")
	}
}

func TestFoldToggleByClick(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	m.SetScreenOrigin(1, 1)
	msg := foldMessage("think1", KindThinking, &Fold{Title: "Thought", Count: 1})
	m.Update(NewMessageMsg{Message: msg})

	// The summary is the first content row: screen (1,1) is list row 0.
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	if !m.isOpen(msg.Fold) {
		t.Fatal("clicking the summary row should expand the fold")
	}
	// A click off the row (below the viewport top, but beyond the fold) is a no-op.
	m.Update(tea.MouseClickMsg{X: 1, Y: 5, Button: tea.MouseLeft})
	if !m.isOpen(msg.Fold) {
		t.Fatal("a click not on the fold row must not toggle")
	}
	// A click outside the list bounds (left of origin) is a no-op.
	m.Update(tea.MouseClickMsg{X: 0, Y: 1, Button: tea.MouseLeft})
	if !m.isOpen(msg.Fold) {
		t.Fatal("a click left of the list must not toggle")
	}
}

// TestFoldToggleByClickOnExpandedMember verifies that clicking a fold's
// expanded member block collapses it again (issue: expanded tool/thinking
// blocks had no fold target, so only the gap between them collapsed the fold).
func TestFoldToggleByClickOnExpandedMember(t *testing.T) {
	m := NewList()
	m.SetSize(80, 20)
	m.SetScreenOrigin(1, 1)
	msg := foldMessage("g1", KindTool, &Fold{
		Title: "read", Count: 1,
		ToolCalls: []ToolCall{
			{CallID: "c1", Name: "read_file", Params: []Param{{Key: "", Value: "a.go"}}, Status: ToolCompleted, Result: "package x"},
		},
	})
	m.Update(NewMessageMsg{Message: msg})

	// Expand the fold (as the user would by clicking the summary).
	m.toggleFold("g1")
	if !m.isOpen(msg.Fold) {
		t.Fatal("fold should be expanded after toggle")
	}
	if len(m.ui) != 1 || m.ui[0].messageType != toolMessageType {
		t.Fatalf("expanded tool fold should render its member block, got %d blocks", len(m.ui))
	}

	// Click the member block itself: screen y = block row + origin offset.
	memberRow := m.ui[0].position
	y := memberRow + 1
	m.Update(tea.MouseClickMsg{X: 1, Y: y, Button: tea.MouseLeft})
	if m.isOpen(msg.Fold) {
		t.Fatal("clicking the expanded member block should collapse the fold")
	}
	// It should render the summary again.
	if len(m.ui) != 1 || m.ui[0].messageType != foldSummaryMessageType {
		t.Fatalf("collapsed fold should render the summary, got %d blocks", len(m.ui))
	}
}

func TestFocusNavigationCyclesFoldRows(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	m.SetAllowUpDown(true)
	for _, id := range []string{"f1", "f2", "f3"} {
		m.Update(NewMessageMsg{Message: foldMessage(id, KindThinking, &Fold{Title: "Thought", Count: 1})})
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.focusID != "f1" {
		t.Fatalf("first down should focus f1, got %q", m.focusID)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.focusID != "f2" {
		t.Fatalf("second down should focus f2, got %q", m.focusID)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.focusID != "f1" {
		t.Fatalf("up should move back to f1, got %q", m.focusID)
	}
	// Wrap around.
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.focusID != "f3" {
		t.Fatalf("up past the first row should wrap to the last, got %q", m.focusID)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.focusID != "f1" {
		t.Fatalf("down past the last row should wrap to the first, got %q", m.focusID)
	}
}

func TestRunningToolGroupAutoExpandsThenCollapses(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	tc := ToolCall{CallID: "c1", Name: "read_file", Status: ToolRunning}
	msg := foldMessage("g1", KindTool, &Fold{
		Title: "read", Count: 1, Running: true, Open: false,
		ToolCalls: []ToolCall{tc},
	})
	m.Update(NewMessageMsg{Message: msg})
	if !m.isOpen(msg.Fold) {
		t.Fatal("a running tool group should auto-expand")
	}

	// Member finishes: the group stops running and folds back.
	tc.Status = ToolCompleted
	msg.Fold.Running = false
	msg.Fold.ToolCalls = []ToolCall{tc}
	m.Update(UpdateMessageMsg{Message: msg})
	if m.isOpen(msg.Fold) {
		t.Fatal("a completed tool group should auto-collapse")
	}

	// The user can still expand a completed group, and it stays expanded.
	m.SetAllowUpDown(true)
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // focus g1
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.isOpen(msg.Fold) {
		t.Fatal("a completed group should be expandable by the user")
	}
	// A running group ignores the toggle (pinned open).
	msg.Fold.Running = true
	tc.Status = ToolRunning
	msg.Fold.ToolCalls = []ToolCall{tc}
	m.Update(UpdateMessageMsg{Message: msg})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.isOpen(msg.Fold) {
		t.Fatal("a running group must stay expanded regardless of toggles")
	}
}

func TestFoldToggleInvalidatesRenderCache(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	msg := foldMessage("think1", KindThinking, &Fold{Title: "Thought", Count: 1})
	m.Update(NewMessageMsg{Message: msg})

	collapsed := m.View().Content
	if !strings.Contains(collapsed, "▸ Thought") {
		t.Fatalf("collapsed fold should render its summary line, got %q", collapsed)
	}
	m.SetAllowUpDown(true)
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	expanded := m.View().Content
	if strings.Contains(expanded, "▸ Thought") {
		t.Fatalf("expanded fold must not render the summary line, got %q", expanded)
	}
	if collapsed == expanded {
		t.Fatal("toggling a fold must change the rendered output")
	}
}

func TestFoldToggleByClickWithScrolledViewport(t *testing.T) {
	m := NewList()
	m.SetSize(80, 5)
	m.SetScreenOrigin(1, 1)
	// Overflow the viewport with user messages, then place the fold in the
	// middle so clicking requires the screen-origin + YOffset mapping.
	for i := 0; i < 12; i++ {
		m.Update(NewMessageMsg{Message: Message{ID: fmt.Sprintf("u%d", i), Kind: KindUser, Content: fmt.Sprintf("line %d", i)}})
	}
	msg := foldMessage("think1", KindThinking, &Fold{Title: "Thought", Count: 1})
	m.Update(NewMessageMsg{Message: msg})
	m.Update(NewMessageMsg{Message: Message{ID: "u13", Kind: KindUser, Content: "tail"}})

	// Find the fold's content row and click its screen position.
	row, ok := m.foldRows["think1"]
	if !ok {
		t.Fatal("fold row not tracked")
	}
	y := row - m.viewport.YOffset() + 1 // +1: screen origin offset
	m.Update(tea.MouseClickMsg{X: 1, Y: y, Button: tea.MouseLeft})
	if !m.isOpen(msg.Fold) {
		t.Fatalf("click at content row %d (screen y=%d) should expand the fold", row, y)
	}
}

// SetMessagesMsg replaces the whole transcript in slice order and clears stale
// per-message state — the /resume path must never show scrambled order.
func TestSetMessagesMsgReplacesInOrder(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	// Seed a stale transcript with a fold, then replace it entirely.
	m.Update(NewMessageMsg{Message: Message{ID: "old", Kind: KindUser, Content: "stale"}})
	m.Update(NewMessageMsg{Message: foldMessage("oldfold", KindThinking, &Fold{Title: "Thought", Count: 1})})

	msgs := []Message{
		{ID: "m1", Kind: KindUser, Content: "u1"},
		{ID: "m2", Kind: KindAssistant, Content: "a1", Finished: true},
		{ID: "m3", Kind: KindUser, Content: "u2"},
		{ID: "m4", Kind: KindAssistant, Content: "a2", Finished: true},
	}
	m.Update(SetMessagesMsg{Messages: msgs})
	if len(m.messages) != 4 {
		t.Fatalf("SetMessagesMsg should install 4 messages, got %d", len(m.messages))
	}
	for i, want := range msgs {
		if m.messages[i].ID != want.ID {
			t.Fatalf("message %d = %q, want %q — order must be preserved", i, m.messages[i].ID, want.ID)
		}
	}
	if len(m.foldState) != 0 || m.focusID != "" || m.workingID != "" {
		t.Fatalf("SetMessagesMsg must clear stale state: fold=%v focus=%q working=%q",
			m.foldState, m.focusID, m.workingID)
	}
}

// BatchMessagesMsg applies one event's messages in slice order — a non-streamed
// final answer followed by its cost footer must keep that order.
func TestBatchMessagesMsgAppliesInSliceOrder(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	m.Update(BatchMessagesMsg{Messages: []Message{
		{ID: "a1", Kind: KindAssistant, Content: "answer", Finished: true},
		{ID: "f1", Kind: KindSystem, Content: "⤷ footer"},
	}})
	if len(m.messages) != 2 || m.messages[0].ID != "a1" || m.messages[1].ID != "f1" {
		t.Fatalf("batch must apply in slice order: %+v", m.messages)
	}
}
