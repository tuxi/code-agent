package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"code-agent/internal/session"
)

func TestCommandToken(t *testing.T) {
	cases := map[string]string{
		"/help":         "/help",
		"/use deepseek": "/use",
		"  /sessions":   "/sessions",
		"hello":         "",  // not a command
		"/a\nb":         "",  // multi-line is never a command
		"/":             "/", // bare slash → token "/"
	}
	for in, want := range cases {
		if got := commandToken(in); got != want {
			t.Errorf("commandToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterCommands(t *testing.T) {
	if got := filterCommands("/"); len(got) != len(commandRegistry) {
		t.Fatalf("bare slash should list all %d commands, got %d", len(commandRegistry), len(got))
	}
	got := filterCommands("/se")
	if len(got) != 1 || got[0].name != "/sessions" {
		t.Fatalf("/se should match only /sessions, got %v", got)
	}
	if got := filterCommands("hello"); got != nil {
		t.Fatalf("non-slash input should yield no commands, got %v", got)
	}
}

func TestCommandArgs(t *testing.T) {
	if got := commandArgs("/use deepseek"); got != "deepseek" {
		t.Fatalf("args = %q", got)
	}
	if got := commandArgs("/help"); got != "" {
		t.Fatalf("no args expected, got %q", got)
	}
}

func TestQuitAliasResolves(t *testing.T) {
	if _, ok := lookupCommand("/quit"); !ok {
		t.Fatal("/quit should resolve to /exit via alias")
	}
	got := filterCommands("/quit")
	if len(got) != 1 || got[0].name != "/exit" {
		t.Fatalf("/quit should match the /exit command, got %v", got)
	}
}

// A slash command is intercepted and run — never sent to the agent as a turn
// (the live bug: /resume was being submitted as a chat message).
func TestSlashCommandIsNotSentAsMessage(t *testing.T) {
	m := readyModel(t)
	m.src.list = func() []session.Meta { return nil }

	// The session dialog's Init() returns nil (no cmd to run), so the dialog
	// being open is the signal.
	m.submit("/resume")
	if m.dialog == nil {
		t.Fatal("/resume should open the session dialog")
	}
	select {
	case got := <-m.b.inputs:
		t.Fatalf("a command must not be sent to the runner, got %q", got)
	default:
	}
	if m.busy {
		t.Fatal("running a command should not lock the composer")
	}
}

// Quitting goes through the ctrl+c confirm dialog: 'y' answers yes.
func TestExitCommandQuits(t *testing.T) {
	m := readyModel(t)
	tm, _ := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = asModel(t, tm)
	if m.dialog == nil {
		t.Fatal("ctrl+c should open the quit dialog")
	}
	tm, cmd := m.Update(tea.KeyPressMsg{Code: 'y'})
	m = asModel(t, tm)
	if cmd == nil {
		t.Fatal("'y' on the quit dialog should return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("'y' should quit the program")
	}
}
