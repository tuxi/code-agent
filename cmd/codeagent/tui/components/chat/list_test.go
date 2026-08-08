package chat

import (
	"fmt"
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
