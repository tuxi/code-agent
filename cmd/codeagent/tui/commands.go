package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// command is one slash command. It is a registry entry (data + a Run func), not a
// growing switch — so aliases, and later fuzzy match / arg schemas / richer
// autocomplete, are added as fields without touching a dispatch block. ready=false
// means it is listed (discoverable) but not yet wired: Run posts a deferral notice.
type command struct {
	name    string
	aliases []string
	desc    string
	ready   bool
	run     func(m *model, args string) tea.Cmd
}

// commandRegistry is the menu. Session-mutating commands (resume/use) are listed
// but deferred — see the design doc; they swap the session/model between turns.
// Commands print to scrollback (tea.Println) or return a program command
// (tea.Quit / tea.ClearScreen) — output is part of the transcript, not a
// separate mutable view.
var commandRegistry = []command{
	{name: "/help", desc: "show commands and key bindings", ready: true,
		run: func(m *model, _ string) tea.Cmd { return tea.Println(helpText) }},
	{name: "/sessions", desc: "list saved sessions", ready: true,
		run: func(m *model, _ string) tea.Cmd { return tea.Println(m.sessions()) }},
	{name: "/model", desc: "show the current model", ready: true,
		run: func(m *model, _ string) tea.Cmd { return tea.Println("model: " + m.header.Model) }},
	{name: "/clear", desc: "clear the screen", ready: true,
		run: func(m *model, _ string) tea.Cmd { return tea.ClearScreen }},
	{name: "/resume", desc: "resume a saved session", ready: true,
		run: func(m *model, args string) tea.Cmd { return m.openResume(args) }},
	{name: "/use", desc: "switch model", ready: false, run: deferNotice},
	{name: "/exit", aliases: []string{"/quit"}, desc: "quit", ready: true,
		run: func(m *model, _ string) tea.Cmd { return tea.Quit }},
}

func deferNotice(_ *model, _ string) tea.Cmd {
	return tea.Println("that command isn't wired in the TUI yet — relaunch: codeagent resume <id>  /  codeagent --model NAME tui")
}

// matches reports whether the command's name or any alias starts with tok.
func (c command) matches(tok string) bool {
	if strings.HasPrefix(c.name, tok) {
		return true
	}
	for _, a := range c.aliases {
		if strings.HasPrefix(a, tok) {
			return true
		}
	}
	return false
}

// commandToken is the first whitespace-delimited token of the composer value
// (e.g. "/use deepseek" → "/use"). Empty if the value is not slash-prefixed.
func commandToken(value string) string {
	value = strings.TrimLeft(value, " ")
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "\n") {
		return ""
	}
	if i := strings.IndexByte(value, ' '); i >= 0 {
		return value[:i]
	}
	return value
}

// commandArgs is everything after the first token (e.g. "/use deepseek" →
// "deepseek"). Empty when the command has no arguments.
func commandArgs(value string) string {
	value = strings.TrimLeft(value, " ")
	if i := strings.IndexByte(value, ' '); i >= 0 {
		return strings.TrimSpace(value[i+1:])
	}
	return ""
}

// filterCommands returns the commands whose name or alias starts with the typed
// token — what the palette shows, and the gate on whether it shows at all.
func filterCommands(value string) []command {
	tok := commandToken(value)
	if tok == "" {
		return nil
	}
	var out []command
	for _, c := range commandRegistry {
		if c.matches(tok) {
			out = append(out, c)
		}
	}
	return out
}

// lookupCommand returns the command matching name exactly (by name or alias).
func lookupCommand(name string) (command, bool) {
	for _, c := range commandRegistry {
		if c.name == name {
			return c, true
		}
		for _, a := range c.aliases {
			if a == name {
				return c, true
			}
		}
	}
	return command{}, false
}

const helpText = `Commands
  /help        show this help
  /sessions    list saved sessions
  /model       show the current model
  /clear       clear the timeline view (session history is kept)
  /resume      resume a saved session (relaunch: codeagent resume <id>)
  /use         switch model (relaunch: codeagent --model NAME tui)
  /exit /quit  leave the workspace

Keys
  enter            send  ·  alt+enter / ctrl+j  newline
  tab              switch focus to the timeline (and back)
  ↑/↓ or j/k       move the timeline cursor (when focused)
  enter            expand / collapse the focused card
  pgup/pgdn        scroll  ·  ctrl+z suspend (fg resumes)  ·  ctrl+c quit
  / at line start  open this command menu`
