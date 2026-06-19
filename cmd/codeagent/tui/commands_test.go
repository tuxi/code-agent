package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

// A slash command is intercepted and run — never sent to the agent as a turn
// (the live bug: /resume was being submitted as a chat message).
func TestSlashCommandIsNotSentAsMessage(t *testing.T) {
	m := readyModel(t)
	m.composer.SetValue("/resume")
	if !m.paletteActive() {
		t.Fatal("/resume should open the command palette")
	}
	m = asModel(t, must(m.Update(tea.KeyMsg{Type: tea.KeyEnter})))

	select {
	case got := <-m.b.inputs:
		t.Fatalf("a command must not be sent to the runner, got %q", got)
	default:
	}
	if m.busy {
		t.Fatal("running a command should not lock the composer")
	}
	if m.composer.Value() != "" {
		t.Fatalf("composer should be cleared after a command, got %q", m.composer.Value())
	}
}

func TestExitCommandQuits(t *testing.T) {
	m := readyModel(t)
	m.composer.SetValue("/exit")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/exit should return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("/exit should quit the program")
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

func TestComposerAutoGrowsAndShrinks(t *testing.T) {
	m := readyModel(t)
	m.composer.SetValue("one\ntwo\nthree")
	m.syncComposer()
	if m.composerHeight != 3 {
		t.Fatalf("composer should grow to 3 rows, got %d", m.composerHeight)
	}
	m.composer.SetValue("")
	m.syncComposer()
	if m.composerHeight != minComposerLines {
		t.Fatalf("composer should shrink back to %d, got %d", minComposerLines, m.composerHeight)
	}
}

func TestCtrlZSuspends(t *testing.T) {
	m := readyModel(t)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
	if cmd == nil {
		t.Fatal("ctrl+z should return a command")
	}
	if _, ok := cmd().(tea.SuspendMsg); !ok {
		t.Fatal("ctrl+z should suspend the program")
	}
}
