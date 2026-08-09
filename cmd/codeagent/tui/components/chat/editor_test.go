package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	chAnsi "github.com/charmbracelet/x/ansi"
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

// Shift+Enter inserts a newline at the cursor without sending; plain Enter
// still sends. The trailing-backslash + Enter escape hatch remains.
func TestShiftEnterInsertsNewline(t *testing.T) {
	e := NewEditor()
	e.SetSize(80, 5)
	e.textarea.SetValue("hello")
	e.textarea.CursorEnd()

	// Shift+Enter: newline inserted, message NOT sent.
	u, cmd := e.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	e = u.(*Editor)
	if cmd != nil {
		t.Fatal("shift+enter must not send the message")
	}
	if e.textarea.Value() != "hello\n" {
		t.Fatalf("shift+enter should insert a newline, got %q", e.textarea.Value())
	}

	// Plain Enter: sends.
	u, cmd = e.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	e = u.(*Editor)
	if cmd == nil {
		t.Fatal("plain enter must send the message")
	}
}

// Typing "/" opens the inline command menu, filtered as more of the prefix is
// typed; up/down move the selection; enter runs the selected command; esc
// closes the menu.
func TestSlashCommandMenu(t *testing.T) {
	e := NewEditor()
	e.SetSize(80, 5)
	e.SetCommands([]Command{
		{ID: "help", Title: "/help", Description: "keyboard shortcuts"},
		{ID: "resume", Title: "/resume", Description: "switch session"},
		{ID: "sessions", Title: "/sessions", Description: "list sessions"},
	})	// Typing "/" opens the menu listing all commands.
	e.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if !e.completion {
		t.Fatal("typing / should open the command menu")
	}
	if len(e.matchedCommands()) != 3 {
		t.Fatalf("all commands should match on bare /, got %d", len(e.matchedCommands()))
	}

	// Typing "re" filters to /resume.
	e.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	e.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	matches := e.matchedCommands()
	if len(matches) != 1 || matches[0].Title != "/resume" {
		t.Fatalf("filter /re should leave /resume, got %+v", matches)
	}

	// Enter runs the selected command: a SendMsg carrying "/resume" is emitted
	// (send() clears the composer by design).
	u, cmd := e.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	e = u.(*Editor)
	if cmd == nil {
		t.Fatal("enter on a menu item must run the command")
	}
	msg := cmd()
	if s, ok := msg.(SendMsg); !ok || s.Text != "/resume" {
		t.Fatalf("enter on menu item should send /resume, got %#v", msg)
	}
	if e.completion {
		t.Fatal("menu should close after selection")
	}

	// Esc closes the menu without sending.
	e.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	u, cmd = e.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	e = u.(*Editor)
	if e.completion {
		t.Fatal("esc should close the menu")
	}
	if cmd != nil {
		t.Fatal("esc should not send anything")
	}
}

// Selecting a NeedsArg command (e.g. /goal) fills the composer with "/cmd "
// and waits for the argument instead of executing immediately; the user types
// the objective and Enter sends the full "/goal <text>".
func TestSlashCommandMenuNeedsArg(t *testing.T) {
	e := NewEditor()
	e.SetSize(80, 5)
	e.SetCommands([]Command{
		{ID: "goal", Title: "/goal", Description: "run a goal", NeedsArg: true},
	})

	e.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if len(e.matchedCommands()) != 1 {
		t.Fatalf("expected /goal to match, got %+v", e.matchedCommands())
	}

	// Enter on the NeedsArg command: no SendMsg, composer keeps "/goal " for
	// the user to type the objective.
	u, cmd := e.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	e = u.(*Editor)
	if cmd != nil {
		t.Fatal("selecting a NeedsArg command must not execute yet")
	}
	if e.textarea.Value() != "/goal " {
		t.Fatalf("composer should hold '/goal ' for the argument, got %q", e.textarea.Value())
	}
	if e.completion {
		t.Fatal("menu should close after selecting the command")
	}

	// The user types the objective, then Enter sends the full command.
	e.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	e.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	e.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	u, cmd = e.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	e = u.(*Editor)
	if cmd == nil {
		t.Fatal("enter with the argument filled should send")
	}
	msg := cmd()
	if s, ok := msg.(SendMsg); !ok || s.Text != "/goal bug" {
		t.Fatalf("expected SendMsg /goal bug, got %#v", msg)
	}
}

// Dragging across the composer selects text and copies it to the clipboard on
// release; a plain click moves the textarea cursor instead. The editor's top
// border is row 0, so content starts at screen y=1 (origin 0,0).
func TestComposerDragSelectCopies(t *testing.T) {
	e := NewEditor()
	e.SetSize(80, 5)
	e.SetScreenOrigin(0, 0)
	var copied string
	e.copyText = func(s string) error { copied = s; return nil }
	e.textarea.SetValue("hello world")
	e.syncComposer()

	// Press at column 0 of the value ("h"), drag to column 5 (" ").
	e.Update(tea.MouseClickMsg{X: 3, Y: 1, Button: tea.MouseLeft}) // "> " + " " prefix = 3
	e.Update(tea.MouseMotionMsg{X: 8, Y: 1, Button: tea.MouseLeft})
	e.Update(tea.MouseReleaseMsg{X: 8, Y: 1, Button: tea.MouseLeft})
	if copied != "hello" {
		t.Fatalf("copied %q, want %q", copied, "hello")
	}

	// A plain click (no drag) moves the cursor, does not copy.
	copied = ""
	e.Update(tea.MouseClickMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	e.Update(tea.MouseReleaseMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	if copied != "" {
		t.Fatalf("a click must not copy, got %q", copied)
	}
	if e.selecting || e.dragged {
		t.Fatal("click should end the drag-selection state")
	}
}

// posToOffset maps screen coordinates onto value byte offsets, handling the
// composer prompt prefix and multi-line values.
func TestComposerPosToOffset(t *testing.T) {
	e := NewEditor()
	e.SetSize(80, 5)
	e.SetScreenOrigin(0, 0)
	e.textarea.SetValue("abc\ndef")
	e.syncComposer()

	// "abc" starts at value offset 0: screen x = 3 ("> " + " "), y = 1.
	if off := e.posToOffset(3, 1); off != 0 {
		t.Fatalf("pos (3,1) should be offset 0, got %d", off)
	}
	// "b" is offset 1: screen x = 4.
	if off := e.posToOffset(4, 1); off != 1 {
		t.Fatalf("pos (4,1) should be offset 1, got %d", off)
	}
	// Second line "def" starts after "abc\n": offset 4, screen y = 2.
	if off := e.posToOffset(3, 2); off != 4 {
		t.Fatalf("pos (3,2) should be offset 4, got %d", off)
	}
	// Outside the editor (left of origin, or below content) is invalid.
	if off := e.posToOffset(-1, 1); off != -1 {
		t.Fatalf("negative x should be invalid, got %d", off)
	}
	if off := e.posToOffset(3, 99); off != -1 {
		t.Fatalf("below-content y should be invalid, got %d", off)
	}
}

// highlightSelection must not mangle the ANSI-styled composer: it strips each
// row first, wraps the selected columns in reverse video, and preserves the
// row prefixes ("> " / padding). Stripping the result must equal the original
// plain text with no escape-sequence garbage.
func TestComposerHighlightSelection(t *testing.T) {
	e := NewEditor()
	e.SetSize(80, 5)
	e.SetScreenOrigin(0, 0)
	e.textarea.SetValue("hello world")
	e.syncComposer()

	// Select "world" (value offsets 6..11).
	e.selStart, e.selCur = 6, 11
	e.selecting, e.dragged = true, true

	view := e.View().Content
	// The reverse-video markers must be present.
	if !strings.Contains(view, "\x1b[7m") || !strings.Contains(view, "\x1b[27m") {
		t.Fatalf("selection highlight missing reverse video: %q", view)
	}
	// Stripping the highlighted view must yield clean text: the value and the
	// prompt intact, no escape-sequence fragments leaking into visible columns.
	plain := chAnsi.Strip(view)
	if !strings.Contains(plain, "> hello world") {
		t.Fatalf("stripped highlight lost text: %q", plain)
	}
	for _, ln := range strings.Split(plain, "\n") {
		if strings.HasPrefix(ln, "\x1b") || strings.Contains(ln, "[38") || strings.Contains(ln, "[48") {
			t.Fatalf("highlight leaked escape fragments into text: %q", ln)
		}
	}
}

// CJK characters occupy two screen columns but one rune. Selecting two CJK
// chars must highlight exactly those two runes (4 screen columns), not truncate
// to one — the selection range is in rune offsets, never columns.
func TestComposerHighlightSelectionCJK(t *testing.T) {
	e := NewEditor()
	e.SetSize(80, 5)
	e.SetScreenOrigin(0, 0)
	e.textarea.SetValue("你好世界")
	e.syncComposer()

	// Select the first two CJK chars "你好" (byte offsets 0..6).
	e.selStart, e.selCur = 0, len([]byte("你好"))
	e.selecting, e.dragged = true, true

	view := e.View().Content
	if !strings.Contains(view, "\x1b[7m你好\x1b[27m") {
		t.Fatalf("CJK selection should wrap exactly two chars in reverse video: %q", view)
	}
	// Stripping must yield the intact value.
	plain := chAnsi.Strip(view)
	if !strings.Contains(plain, "> 你好世界") {
		t.Fatalf("stripped highlight lost CJK text: %q", plain)
	}
}
