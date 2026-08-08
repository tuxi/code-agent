package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestDragSelectCopiesText exercises the full flow: left-press, drag, release
// copies the selected cells (borders and padding stripped) to the clipboard,
// with a reverse-video highlight while dragging that clears on release.
func TestDragSelectCopiesText(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	m.SetScreenOrigin(1, 1)
	var copied string
	m.copyText = func(s string) error { copied = s; return nil }

	m.Update(NewMessageMsg{Message: Message{ID: "u1", Kind: KindUser, Content: "hello world"}})
	m.Update(NewMessageMsg{Message: Message{ID: "a1", Kind: KindAssistant, Content: "answer one", Finished: true}})

	// "hello world" starts at content col 2 on row 0 (border + padding space).
	m.Update(tea.MouseClickMsg{X: 3, Y: 1, Button: tea.MouseLeft})     // press at col 2
	m.Update(tea.MouseMotionMsg{X: 14, Y: 1, Button: tea.MouseLeft})   // drag to col 13
	if !strings.Contains(m.viewport.View(), "\x1b[7m") {
		t.Fatal("selection should render a reverse-video highlight")
	}
	m.Update(tea.MouseReleaseMsg{X: 14, Y: 1, Button: tea.MouseLeft})
	if copied != "hello world" {
		t.Fatalf("copied %q, want %q", copied, "hello world")
	}
	if strings.Contains(m.viewport.View(), "\x1b[7m") {
		t.Fatal("selection highlight should clear after release")
	}
}

// TestDragSelectCopiesMultiline verifies a multi-row selection joins the lines
// with newlines and trims the right padding.
func TestDragSelectCopiesMultiline(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	m.SetScreenOrigin(1, 1)
	var copied string
	m.copyText = func(s string) error { copied = s; return nil }

	m.Update(NewMessageMsg{Message: Message{ID: "s1", Kind: KindSystem, Content: "line one\nline two"}})

	// Select from row 0 col 0 to row 1 (drag to the right edge).
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 80, Y: 2, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 80, Y: 2, Button: tea.MouseLeft})
	if copied != "line one\nline two" {
		t.Fatalf("copied %q, want %q", copied, "line one\nline two")
	}
}

// TestDragSelectCopiesWideChars verifies CJK-wide cells are selected by cell
// column and copied without splitting or padding artifacts.
func TestDragSelectCopiesWideChars(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	m.SetScreenOrigin(1, 1)
	var copied string
	m.copyText = func(s string) error { copied = s; return nil }

	m.Update(NewMessageMsg{Message: Message{ID: "s1", Kind: KindSystem, Content: "你好世界"}})

	// Four wide chars occupy cells 0..7; drag to col 8 selects all of them.
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 9, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 9, Y: 1, Button: tea.MouseLeft})
	if copied != "你好世界" {
		t.Fatalf("copied %q, want %q", copied, "你好世界")
	}
}

// TestDragAcrossFoldSelectsNotToggle verifies that a drag over a fold summary
// copies text instead of expanding/collapsing the fold.
func TestDragAcrossFoldSelectsNotToggle(t *testing.T) {
	m := NewList()
	m.SetSize(80, 10)
	m.SetScreenOrigin(1, 1)
	var copied string
	m.copyText = func(s string) error { copied = s; return nil }

	msg := foldMessage("think1", KindThinking, &Fold{Title: "Thought", Count: 1})
	m.Update(NewMessageMsg{Message: msg})
	if m.isOpen(msg.Fold) {
		t.Fatal("fold should start collapsed")
	}

	// Drag across the summary row (row 0): press col 0, release col 11.
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 12, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 12, Y: 1, Button: tea.MouseLeft})
	if m.isOpen(msg.Fold) {
		t.Fatal("a drag over a fold must not toggle it")
	}
	if copied == "" {
		t.Fatal("a drag should copy the selected text")
	}
}
