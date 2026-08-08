package tui

import "strings"

// command is one slash command: a registry entry (data only). Execution lives in
// app.go — the runCommand switch and the ctrl+k dialog's commandList() — so the
// registry exists to back the pure lookup/filter helpers below (composer text
// path and tests). Keep commandRegistry in sync with app.go's runCommand cases.
type command struct {
	name    string
	aliases []string
	desc    string
}

// commandRegistry is the command menu. It mirrors app.go's commandList(); adding
// a command means touching both, since the dialog needs a handler and the
// composer path needs a dispatch case.
var commandRegistry = []command{
	{name: "/help", desc: "show commands and key bindings"},
	{name: "/sessions", desc: "list saved sessions"},
	{name: "/model", desc: "show the current model"},
	{name: "/clear", desc: "clear the screen"},
	{name: "/resume", desc: "resume a saved session"},
	{name: "/use", desc: "switch to another configured model"},
	{name: "/auto", desc: "toggle auto-approval (edits auto-approved, commands confirmed)"},
	{name: "/goal", desc: "pursue an objective (no arg: status · resume · clear)"},
	{name: "/prompts", desc: "list MCP prompts (invoke as /mcp__server__prompt)"},
	{name: "/exit", aliases: []string{"/quit"}, desc: "quit"},
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
// token — the gate on whether the composer treats the input as a command.
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

// onOff renders a boolean as an uppercase ON/OFF label for /auto status lines.
func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}
