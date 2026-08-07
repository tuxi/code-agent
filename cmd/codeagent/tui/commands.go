package tui

import (
	"fmt"
	"strings"
	"time"

	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/internal/session"

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
// Commands print to scrollback (appendLines) or return a program command
// (tea.Quit / tea.ClearScreen) — output is part of the transcript, not a
// separate mutable view.
var commandRegistry = []command{
	{name: "/help", desc: "show commands and key bindings", ready: true,
		run: func(m *model, _ string) tea.Cmd { return appendLines(helpText) }},
	{name: "/sessions", desc: "list saved sessions", ready: true,
		run: func(m *model, _ string) tea.Cmd { return appendLines(m.sessions()) }},
	{name: "/model", desc: "show the current model", ready: true,
		run: func(m *model, _ string) tea.Cmd { return appendLines("model: " + m.header.Model) }},
	{name: "/clear", desc: "clear the screen", ready: true,
		run: func(m *model, _ string) tea.Cmd { return tea.ClearScreen }},
	{name: "/resume", desc: "resume a saved session", ready: true,
		run: func(m *model, args string) tea.Cmd { return m.openResume(args) }},
	{name: "/use", desc: "switch to another configured model", ready: true,
		run: func(m *model, args string) tea.Cmd { return m.openUse(args) }},
	{name: "/auto", desc: "auto-approve in-workspace edits (commands still confirmed)", ready: true,
		run: func(m *model, args string) tea.Cmd { return m.toggleAuto(args) }},
	{name: "/goal", desc: "pursue an objective (no arg: status · resume · clear)", ready: true,
		run: func(m *model, args string) tea.Cmd { return m.goalDispatch(args) }},
	{name: "/prompts", desc: "list MCP prompts (invoke as /mcp__server__prompt)", ready: true,
		run: func(m *model, _ string) tea.Cmd { return appendLines(m.promptHelp()) }},
	{name: "/exit", aliases: []string{"/quit"}, desc: "quit", ready: true,
		run: func(m *model, _ string) tea.Cmd { return tea.Quit }},
}

func deferNotice(_ *model, _ string) tea.Cmd {
	return appendLines("that command isn't wired in the TUI yet — relaunch: codeagent resume <id>  /  codeagent --model NAME tui")
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
  /clear       clear the screen
  /resume      resume a saved session
  /use         switch to another configured model
  /auto        on|off — auto-approve in-workspace edits (commands still confirmed)
  /goal        <objective> to start · /goal (status) · /goal resume · /goal clear  (/auto on for hands-off)
  /exit /quit  leave the workspace

Keys
  enter            send  ·  alt+enter / ctrl+j  newline
  ctrl+z           suspend (fg resumes)  ·  ctrl+c quit
  / at line start  open this command menu`

// --- command actions -----------------------------------------------------

// runCommand looks the command up in the registry and runs it — no dispatch
// switch, so new commands are added in commands.go alone. Pointer receiver:
// the command's Run mutates the model (composer reset, opening an overlay), so
// it must act on the model the key dispatch is about to return.
func (m *model) runCommand(name, args string) tea.Cmd {
	m.composer.Reset()
	m.syncComposer()
	m.palette.cmdIdx = 0
	m.lastEsc = time.Time{}
	cmd, ok := lookupCommand(name)
	if !ok {
		return appendLines("unknown command: " + name)
	}
	return cmd.run(m, args)
}

func (m model) sessions() string {
	if m.src.list == nil {
		return "no saved sessions"
	}
	return formatSessionList(m.src.list())
}

// promptHelp lists the available MCP prompts for the /prompts command.
func (m model) promptHelp() string {
	if m.src.promptOps == nil {
		return "(no MCP prompts available)"
	}
	return m.src.promptOps.Help()
}

// toggleAuto flips auto-approval (the shared AutoApprover). SetEnabled is an
// atomic on shared state, so it is safe to call from the render goroutine without
// going through the run loop.
func (m model) toggleAuto(args string) tea.Cmd {
	if m.src.auto == nil {
		return appendLines("auto mode is not available in this session")
	}
	switch strings.TrimSpace(args) {
	case "on":
		m.src.auto.SetEnabled(true)
	case "off":
		m.src.auto.SetEnabled(false)
	case "":
		return appendLines("auto mode is " + onOff(m.src.auto.Enabled()) + " (usage: /auto on|off)")
	default:
		return appendLines("usage: /auto [on|off]")
	}
	if m.src.auto.Enabled() {
		return appendLines("auto mode ON — in-workspace edits (edit_file/create_file) auto-approved; commands, patches, and commits still confirmed.")
	}
	return appendLines("auto mode OFF — every side-effecting tool is confirmed again.")
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// goalDispatch routes a /goal command: no-arg/status → status, clear → clear,
// resume → resume the existing goal, anything else → start a pursuit with that
// objective. All forms refuse while busy (Ctrl-C pauses a running pursuit first).
func (m *model) goalDispatch(args string) tea.Cmd {
	if m.busy {
		return appendLines("a pursuit is running — Ctrl-C to pause it first")
	}
	switch a := strings.TrimSpace(args); a {
	case "", "status":
		return m.goalCtl(ctlStatus)
	case "clear":
		return m.goalCtl(ctlClear)
	case "resume":
		return m.startPursuit("") // "" resumes the session's existing goal
	default:
		return m.startPursuit(a)
	}
}

// startPursuit kicks off a pursuit (or resume, when obj == "") on the run-loop
// goroutine and locks the composer until it finishes (goalDoneMsg). The pursuit's
// turns render live through the event stream; ctrl+c pauses it (CancelTurn).
func (m *model) startPursuit(obj string) tea.Cmd {
	m.busy = true
	m.lastErr = nil
	b := m.b
	return func() tea.Msg { b.goalStart <- obj; return nil }
}

// goalCtl runs a quick status/clear op on the run loop and prints the reply. The
// blocking receive is fine: it runs in a tea.Cmd goroutine, and these are only
// issued when idle (the run loop's select handles them at once).
func (m model) goalCtl(kind int) tea.Cmd {
	b := m.b
	return func() tea.Msg {
		reply := make(chan string, 1)
		b.goalCtl <- goalCtlReq{kind: kind, reply: reply}
		return goalCtlResultMsg(<-reply)
	}
}

// --- /resume picker -----------------------------------------------------

// openResume opens the session picker (no arg) or resumes a session directly
// (with an id). Refuses mid-turn — the swap lands at a turn boundary anyway.
func (m *model) openResume(args string) tea.Cmd {
	if m.busy {
		return appendLines("finish the current turn before resuming")
	}
	if args != "" {
		return m.resume(session.Meta{ID: args})
	}
	if m.src.list == nil {
		return appendLines("no saved sessions")
	}
	metas := m.src.list()
	if len(metas) == 0 {
		return appendLines("no saved sessions")
	}
	m.overlay = &sessionPickerOverlay{metas: metas}
	return nil
}

// maxReplayLines bounds the resumed-history dump so a huge session doesn't flood
// the terminal; the tail (most recent) is kept and the full conversation is still
// loaded into context.
const maxReplayLines = 300

// resume loads the chosen session, hands it to the run loop (swapped in at the
// next turn boundary), replays its history to scrollback, and updates the
// header/gauge.
func (m *model) resume(meta session.Meta) tea.Cmd {
	m.overlay = nil
	if m.src.resume == nil {
		return appendLines("resume not available")
	}
	sess, err := m.src.resume(meta.ID)
	if err != nil {
		return appendLines("resume failed: " + err.Error())
	}
	m.b.sessSwap <- sess // buffered (cap 1); the run loop applies it between turns
	m.header.Session = sess.ID
	m.promptTokens = sess.PromptTokens
	m.skills = map[string]bool{}
	m.tr = transcript{} // fresh transcript for the resumed session's new turns

	title := sessionTitle(meta.Title)
	if title == "" {
		title = sess.ID
	}
	lines := []string{theme.Default.Meta.Render(fmt.Sprintf("──── resumed: %s · %d messages ────", title, len(sess.Messages)))}

	// Replay the persisted event history through the same transcript renderer, so
	// it reads exactly as it did live. Sessions older than the EventStore have no
	// events — they resume with context intact but no visible back-scroll.
	if m.src.events != nil {
		if hist := renderTranscript(m.src.events(meta.ID), m.width); len(hist) > 0 {
			if len(hist) > maxReplayLines {
				omitted := len(hist) - maxReplayLines
				hist = append([]string{theme.Default.Meta.Render(fmt.Sprintf("… %d earlier lines omitted (full conversation is loaded)", omitted))}, hist[len(hist)-maxReplayLines:]...)
			}
			lines = append(lines, hist...)
			m.tr.started = true // separate the next live turn from the replayed history
		}
	}
	return appendLines(strings.Join(lines, "\n"))
}

// --- /use model picker ---------------------------------------------------

// openUse opens the model picker (no arg) or switches model directly (with a
// name). Refuses mid-turn — the swap lands at a turn boundary via modelSwap.
func (m *model) openUse(args string) tea.Cmd {
	if m.busy {
		return appendLines("finish the current turn before switching models")
	}
	if args != "" {
		return m.useModel(args)
	}
	if len(m.src.modelNames) == 0 {
		return appendLines("no other configured models")
	}
	m.overlay = &modelPickerOverlay{models: m.src.modelNames}
	return nil
}

// useModel sends the model name to the run-loop goroutine (swapped between
// turns), then awaits the result — the same async-safety pattern as /resume.
func (m *model) useModel(name string) tea.Cmd {
	m.overlay = nil
	if m.src.modelSwap == nil {
		return appendLines("model switch not available")
	}
	m.b.modelSwap <- name // buffered; the run loop applies it between turns
	return waitForModelSwapResult(m.b.modelSwapResult)
}
