package chat

import (
	"testing"

	"github.com/mattn/go-runewidth"
)

// The composer auto-grows to fit its content and shrinks back to the IME-safe
// single line when cleared. This behavior moved from the deleted root model
// (m.composerHeight / syncComposer) into the chat.Editor component.
func TestComposerAutoGrowsAndShrinks(t *testing.T) {
	e := NewEditor()
	e.SetSize(80, 5)
	e.textarea.SetValue("one\ntwo\nthree")
	e.syncComposer()
	if e.composerH != 3 {
		t.Fatalf("composer should grow to 3 rows, got %d", e.composerH)
	}
	e.textarea.SetValue("")
	e.syncComposer()
	if e.composerH != minComposerLines {
		t.Fatalf("composer should shrink back to %d, got %d", minComposerLines, e.composerH)
	}
}

// The composer prompt is pure ASCII so its display width equals its rune count —
// that is what keeps the IME candidate placement stable (moved from the deleted
// root model's TestComposerPromptHasStableWidth).
func TestComposerPromptHasStableWidth(t *testing.T) {
	if got, want := runewidth.StringWidth(composerPrompt), len(composerPrompt); got != want {
		t.Fatalf("composerPrompt display width = %d, want byte/rune width %d for stable IME placement", got, want)
	}
}

// composerWidth leaves only the right padding off the terminal width (moved from
// the deleted root model's TestComposerWidthLeavesRightPaddingOnly).
func TestComposerWidthLeavesRightPaddingOnly(t *testing.T) {
	if got := composerWidth(80); got != 79 {
		t.Fatalf("composerWidth(80) = %d, want 79", got)
	}
	if got := composerWidth(1); got != 1 {
		t.Fatalf("composerWidth(1) = %d, want 1", got)
	}
}
