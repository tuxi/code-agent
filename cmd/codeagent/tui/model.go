package tui

import (
	"fmt"
	"strings"

	"code-agent/internal/agent"
	"code-agent/internal/session"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// HeaderInfo is the "where am I" content. The live context gauge needs the
// threshold (the prompt-token count comes off the event stream).
type HeaderInfo struct {
	Model            string
	Workspace        string
	Session          string
	CompactThreshold int
}

const (
	minComposerLines = 1 // composer starts one line tall (cursor on the bottom row → IME-friendly)
	maxComposerLines = 8 // and grows with content up to here, then scrolls internally
)

// model is the inline BubbleTea program (no alt-screen): an "enhanced terminal",
// not a full-screen TUI. Finalized events are printed to the terminal's own
// scrollback (native copy / scroll / search); the live region it redraws is just
// a status line, an optional command palette, and the composer at the very
// bottom. It renders the event stream and owns no control flow — the agent does.
type model struct {
	b      *Backend
	header HeaderInfo

	composer textarea.Model
	spinner  spinner.Model
	// tr renders the agent event stream into the printed transcript — shared with
	// /resume history replay (transcript.go).
	tr transcript

	cmdIdx int            // selected slash-command in the palette
	src    sessionSource  // saved-session access for /sessions and /resume
	picker *sessionPicker // /resume overlay; nil when closed

	pending  *approvalReq // set while a side-effecting tool awaits y/n
	busy     bool         // a turn is running; submit is locked
	thinking bool         // a model call is in flight; show the spinner
	lastErr  error

	promptTokens int             // latest prompt size (from EventModelFinished) for the gauge
	skills       map[string]bool // distinct skills loaded this session

	composerHeight int  // current composer rows (auto-grows with content)
	width          int  // terminal width (for wrapping printed output)
	ready          bool // a WindowSizeMsg has arrived
}

func newModel(b *Backend, header HeaderInfo, src sessionSource) model {
	ta := textarea.New()
	ta.Placeholder = "Ask, paste, or describe a change…  (Enter to send, Alt+Enter for a newline)"
	ta.ShowLineNumbers = false
	ta.Prompt = "┃ "
	ta.CharLimit = 0
	// Edit-first composer: Enter sends (handled in Update), so newline moves to
	// Alt+Enter / Ctrl+J — the cross-terminal-reliable combo.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.MaxHeight = maxComposerLines
	ta.SetHeight(minComposerLines)
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styleSkill

	return model{
		b:              b,
		header:         header,
		composer:       ta,
		spinner:        sp,
		skills:         map[string]bool{},
		src:            src,
		composerHeight: minComposerLines,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		tea.Println(m.banner()),
		textarea.Blink,
		m.spinner.Tick,
		waitForEvent(m.b.events),
		waitForApproval(m.b.approvals),
		waitForDone(m.b.done),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.composer.SetWidth(msg.Width - 2)
		m.ready = true
		return m, nil

	case tea.KeyMsg:
		if m.pending != nil {
			return m.handleApprovalKey(msg)
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+z":
			return m, tea.Suspend // job-control suspend; the shell's `fg` resumes
		}
		if m.picker != nil {
			return m.handlePickerKey(msg)
		}
		if m.paletteActive() {
			if handled, mm, cmd := m.handlePaletteKey(msg); handled {
				return mm, cmd
			}
		}
		if msg.String() == "enter" {
			return m.submit()
		}
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(msg)
		m.syncComposer() // grow/shrink the composer with its content
		return m, cmd

	case eventMsg:
		return m.handleEvent(agent.Event(msg))

	case approvalMsg:
		req := approvalReq(msg)
		m.pending = &req
		return m, waitForApproval(m.b.approvals)

	case doneMsg:
		m.busy = false
		m.thinking = false
		m.lastErr = msg.err
		out := m.tr.flush(m.width) // a turn that errored never sent TurnFinished
		cmds := []tea.Cmd{waitForDone(m.b.done)}
		if len(out) > 0 {
			cmds = append([]tea.Cmd{tea.Println(strings.Join(out, "\n"))}, cmds...)
		}
		return m, tea.Batch(cmds...)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleEvent groups events into steps (a model call + the tools it ran) and
// prints a finished step to scrollback as a "Thought for Ns, read 1 file" header
// with the real commands beneath — so the agent's work is visible without merging
// events or needing an expand. A step flushes when the next model call starts, or
// the turn ends. User prompts, the reply, reflections, and compaction print as
// their own cards.
func (m model) handleEvent(ev agent.Event) (tea.Model, tea.Cmd) {
	// Live UI state (spinner, gauge, skills) — separate from transcript rendering.
	switch ev.Kind {
	case agent.EventModelStarted:
		m.thinking = true
	case agent.EventModelFinished:
		m.thinking = false
		if ev.PromptTokens > 0 {
			m.promptTokens = ev.PromptTokens
		}
	case agent.EventSkillLoaded:
		m.skills[ev.ToolName] = true
	}

	out := m.tr.render(ev, m.width)
	cmds := []tea.Cmd{waitForEvent(m.b.events)}
	if len(out) > 0 {
		cmds = append([]tea.Cmd{tea.Println(strings.Join(out, "\n"))}, cmds...)
	}
	return m, tea.Batch(cmds...)
}

// submit hands the composed input to the runner goroutine and locks the composer
// until the turn finishes (doneMsg). The user prompt re-enters the printed
// transcript as an ItemUser via EventTurnStarted, so there is one source of truth.
func (m model) submit() (tea.Model, tea.Cmd) {
	if m.busy {
		return m, nil
	}
	input := strings.TrimSpace(m.composer.Value())
	if input == "" {
		return m, nil
	}
	m.composer.Reset()
	m.syncComposer()
	m.busy = true
	m.lastErr = nil
	b := m.b
	return m, func() tea.Msg { b.inputs <- input; return nil }
}

func (m model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.pending.reply <- true
		m.pending = nil
	case "n", "N", "esc":
		m.pending.reply <- false
		m.pending = nil
	case "ctrl+c":
		m.pending.reply <- false
		m.pending = nil
		return m, tea.Quit
	}
	return m, nil
}

// --- command palette ----------------------------------------------------

// paletteActive reports whether the slash-command menu should show: no approval
// pending, and the line so far matches at least one command.
func (m model) paletteActive() bool {
	return m.pending == nil && len(filterCommands(m.composer.Value())) > 0
}

// handlePaletteKey drives the command menu. Returns handled=false for keys it
// doesn't own (e.g. typing), so they fall through to the composer.
func (m model) handlePaletteKey(msg tea.KeyMsg) (bool, tea.Model, tea.Cmd) {
	cmds := filterCommands(m.composer.Value())
	m.cmdIdx = clampInt(m.cmdIdx, 0, len(cmds)-1)
	switch msg.String() {
	case "up", "ctrl+p":
		if m.cmdIdx > 0 {
			m.cmdIdx--
		}
		return true, m, nil
	case "down", "ctrl+n":
		if m.cmdIdx < len(cmds)-1 {
			m.cmdIdx++
		}
		return true, m, nil
	case "tab":
		m.composer.SetValue(cmds[m.cmdIdx].name + " ")
		m.cmdIdx = 0
		return true, m, nil
	case "esc":
		m.composer.Reset()
		m.syncComposer()
		m.cmdIdx = 0
		return true, m, nil
	case "enter":
		mm, cmd := m.runCommand(cmds[m.cmdIdx].name, commandArgs(m.composer.Value()))
		return true, mm, cmd
	}
	return false, m, nil
}

// runCommand looks the command up in the registry and runs it — no dispatch
// switch, so new commands are added in commands.go alone.
func (m model) runCommand(name, args string) (tea.Model, tea.Cmd) {
	m.composer.Reset()
	m.syncComposer()
	m.cmdIdx = 0
	cmd, ok := lookupCommand(name)
	if !ok {
		return m, tea.Println("unknown command: " + name)
	}
	return m, cmd.run(&m, args)
}

func (m model) sessions() string {
	if m.src.list == nil {
		return "no saved sessions"
	}
	return formatSessionList(m.src.list())
}

// --- /resume picker -----------------------------------------------------

// openResume opens the session picker (no arg) or resumes a session directly
// (with an id). Refuses mid-turn — the swap lands at a turn boundary anyway.
func (m *model) openResume(args string) tea.Cmd {
	if m.busy {
		return tea.Println("finish the current turn before resuming")
	}
	if args != "" {
		return m.resume(session.Meta{ID: args})
	}
	if m.src.list == nil {
		return tea.Println("no saved sessions")
	}
	metas := m.src.list()
	if len(metas) == 0 {
		return tea.Println("no saved sessions")
	}
	m.picker = &sessionPicker{metas: metas}
	return nil
}

func (m model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.picker
	switch msg.String() {
	case "up", "k", "ctrl+p":
		if p.idx > 0 {
			p.idx--
		}
	case "down", "j", "ctrl+n":
		if p.idx < len(p.metas)-1 {
			p.idx++
		}
	case "enter":
		if len(p.metas) == 0 {
			m.picker = nil
			return m, nil
		}
		return m, m.resume(p.metas[p.idx])
	case "esc":
		m.picker = nil
	}
	return m, nil
}

// maxReplayLines bounds the resumed-history dump so a huge session doesn't flood
// the terminal; the tail (most recent) is kept and the full conversation is still
// loaded into context.
const maxReplayLines = 300

// resume loads the chosen session, hands it to the run loop (swapped in at the
// next turn boundary), replays its history to scrollback, and updates the
// header/gauge.
func (m *model) resume(meta session.Meta) tea.Cmd {
	m.picker = nil
	if m.src.resume == nil {
		return tea.Println("resume not available")
	}
	sess, err := m.src.resume(meta.ID)
	if err != nil {
		return tea.Println("resume failed: " + err.Error())
	}
	m.b.swap <- sess // buffered (cap 1); the run loop applies it between turns
	m.header.Session = sess.ID
	m.promptTokens = sess.PromptTokens
	m.skills = map[string]bool{}
	m.tr = transcript{} // fresh transcript for the resumed session's new turns

	title := sessionTitle(meta.Title)
	if title == "" {
		title = sess.ID
	}
	lines := []string{styleMeta.Render(fmt.Sprintf("──── resumed: %s · %d messages ────", title, len(sess.Messages)))}

	// Replay the persisted event history through the same transcript renderer, so
	// it reads exactly as it did live. Sessions older than the EventStore have no
	// events — they resume with context intact but no visible back-scroll.
	if m.src.events != nil {
		if hist := renderTranscript(m.src.events(meta.ID), m.width); len(hist) > 0 {
			if len(hist) > maxReplayLines {
				omitted := len(hist) - maxReplayLines
				hist = append([]string{styleMeta.Render(fmt.Sprintf("… %d earlier lines omitted (full conversation is loaded)", omitted))}, hist[len(hist)-maxReplayLines:]...)
			}
			lines = append(lines, hist...)
			m.tr.started = true // separate the next live turn from the replayed history
		}
	}
	return tea.Println(strings.Join(lines, "\n"))
}

// syncComposer grows/shrinks the composer to fit its content (1..max rows). A
// one-line composer keeps the cursor on the terminal's bottom row, where the IME
// candidate window has room below it — the root fix for the IME overlap.
func (m *model) syncComposer() {
	n := clampInt(strings.Count(m.composer.Value(), "\n")+1, minComposerLines, maxComposerLines)
	if n != m.composerHeight {
		m.composerHeight = n
		m.composer.SetHeight(n)
	}
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// --- live region --------------------------------------------------------

func (m model) View() string {
	if !m.ready {
		return ""
	}
	lines := []string{m.statusLine()}
	switch {
	case m.picker != nil:
		lines = append(lines, renderPicker(*m.picker, m.width)...)
	case m.paletteActive():
		cmds := filterCommands(m.composer.Value())
		lines = append(lines, renderPalette(cmds, clampInt(m.cmdIdx, 0, len(cmds)-1), m.width)...)
	default:
		lines = append(lines, styleMeta.Render(m.hint()))
	}
	lines = append(lines, m.composer.View()) // composer LAST (IME-friendly)
	return strings.Join(lines, "\n")
}

// statusLine is the one live status row: what's happening on the left, the
// context gauge + skills on the right.
func (m model) statusLine() string {
	var left string
	switch {
	case m.pending != nil:
		return styleFail.Render("⚠ run "+m.pending.tool+"?") + "   " + styleMeta.Render("[y] run   [n] skip")
	case m.thinking:
		left = m.spinner.View() + styleMeta.Render(" thinking…")
	case m.busy:
		left = styleMeta.Render("working…")
	case m.lastErr != nil:
		left = styleFail.Render("error: " + m.lastErr.Error())
	default:
		left = styleMeta.Render("ready")
	}
	var right []string
	if m.header.CompactThreshold > 0 {
		right = append(right, fmt.Sprintf("ctx %s/%s", humanK(m.promptTokens), humanK(m.header.CompactThreshold)))
	}
	if n := len(m.skills); n > 0 {
		right = append(right, fmt.Sprintf("skills %d", n))
	}
	if len(right) == 0 {
		return left
	}
	return left + "   " + styleMeta.Render(strings.Join(right, " · "))
}

func (m model) hint() string {
	return "enter send · alt+enter newline · / commands · ctrl+z suspend (fg resumes) · ctrl+c quit"
}

func (m model) banner() string {
	parts := []string{"CodeAgent"}
	if m.header.Model != "" {
		parts = append(parts, m.header.Model)
	}
	if m.header.Workspace != "" {
		parts = append(parts, m.header.Workspace)
	}
	line := styleAsstLabel.Render(strings.Join(parts, " · "))
	return line + "\n" + styleMeta.Render("type a request, or /help for commands")
}
