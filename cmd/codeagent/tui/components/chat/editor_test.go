package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
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
