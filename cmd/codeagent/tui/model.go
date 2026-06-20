package tui

import (
	"fmt"
	"strings"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/session"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// HeaderInfo is the "where am I" content. The live context gauge needs the
// threshold (the prompt-token count comes off the event stream).
type HeaderInfo struct {
	Model            string
	Workspace        string
	Session          string
	CompactThreshold int
	SubagentBudget   int // the subagent's iteration cap, for the "step N/M" heartbeat
}

const (
	minComposerLines = 1 // composer starts one line tall (cursor on the bottom row → IME-friendly)
	maxComposerLines = 8 // and grows with content up to here, then scrolls internally

	composerPrompt       = "> "
	composerRightPadding = 1
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

	showThinking bool      // ctrl+o toggle: show current step's thinking in the live region (on by default)
	lastEsc      time.Time // double-Esc clears the composer (like Claude Code)

	cmdIdx    int            // selected slash-command in the palette
	src       sessionSource  // saved-session / model access for slash commands
	picker    *sessionPicker // /resume overlay; nil when closed
	modelPick *modelPicker   // /use overlay; nil when closed

	pending     *approvalReq // set while a side-effecting tool awaits y/n
	approveIdx  int          // 0 = approve (y), 1 = deny (n) — ↑/↓ switches
	showPreview bool         // 'v' toggles the diff preview below the approval card
	busy        bool         // a turn is running; submit is locked
	thinking    bool         // a model call is in flight; show the spinner
	lastErr     error

	promptTokens int             // latest prompt size (from EventModelFinished) for the gauge
	skills       map[string]bool // distinct skills loaded this session

	// Subagent heartbeat: a delegated `task` runs in its own session, so its events
	// arrive with a different SessionID. We surface them as a condensed status line
	// (never the transcript — that would re-flood what delegation keeps out).
	subActive bool
	subStep   int    // subagent loop iterations (EventModelStarted count)
	subTool   string // the tool the subagent's current iteration is running

	composerHeight int  // current composer rows (auto-grows with content)
	width          int  // terminal width (for wrapping printed output)
	ready          bool // a WindowSizeMsg has arrived
}

func newModel(b *Backend, header HeaderInfo, src sessionSource) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message…  (/ for commands)"
	ta.ShowLineNumbers = false
	ta.Prompt = composerPrompt
	ta.CharLimit = 0

	// 显式指定这是一个支持多行的组件
	ta.SetHeight(minComposerLines)

	// Edit-first composer: Enter sends (handled in Update), so newline moves to
	// Alt+Enter / Ctrl+J — the cross-terminal-reliable combo.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	// 不要在这里限制 ta.MaxHeight 导致组件内部裁剪逻辑冲突
	//ta.MaxHeight = maxComposerLines

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
		showThinking:   true, // on by default — thinking is the signal, ctrl+o hides it
		composerHeight: minComposerLines,
	}
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		textarea.Blink,
		m.spinner.Tick,
		waitForEvent(m.b.events),
		waitForApproval(m.b.approvals),
		waitForDone(m.b.done),
	}
	// Banner printed once at startup — no git summary here (it follows each turn).
	return tea.Batch(append([]tea.Cmd{tea.Println(m.banner())}, cmds...)...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.composer.SetWidth(composerWidth(msg.Width))
		m.ready = true
		m.syncComposer() // <- 宽度变了，折行数也会变，必须同步！
		return m, nil

	case tea.KeyMsg:
		if m.pending != nil {
			return m.handleApprovalKey(msg)
		}
		switch msg.String() {
		case "ctrl+c":
			if m.busy {
				m.b.CancelTurn() // cancel the in-flight turn; save + done signal follow
				return m, nil
			}
			return m, tea.Quit
		case "ctrl+z":
			return m, tea.Suspend // job-control suspend; the shell's `fg` resumes
		case "ctrl+o":
			m.showThinking = !m.showThinking
		}
		if m.picker != nil {
			return m.handlePickerKey(msg)
		}
		if m.modelPick != nil {
			return m.handleModelPickerKey(msg)
		}
		if m.paletteActive() {
			if handled, mm, cmd := m.handlePaletteKey(msg); handled {
				return mm, cmd
			}
		}
		if msg.String() == "esc" && m.composer.Value() != "" {
			now := time.Now()
			if m.lastEsc.IsZero() || now.Sub(m.lastEsc) > 500*time.Millisecond {
				m.lastEsc = now
				return m, nil
			}
			m.lastEsc = time.Time{}
			m.composer.Reset()
			m.syncComposer()
			return m, nil
		}
		m.lastEsc = time.Time{}
		if msg.String() == "enter" {
			return m.submit()
		}

		// 普通字符输入
		var cmd tea.Cmd
		// 正常的字符输入处理
		m.composer, cmd = m.composer.Update(msg)
		// 根据新内容计算并设置新高度
		// grow/shrink the composer with its content
		m.syncComposer()

		return m, cmd

	case eventMsg:
		return m.handleEvent(agent.Event(msg))

	case approvalMsg:
		req := approvalReq(msg)
		m.pending, m.approveIdx, m.showPreview = &req, 0, false
		return m, waitForApproval(m.b.approvals)

	case modelSwappedMsg:
		if msg.err != nil {
			return m, tea.Println(styleFail.Render("model switch failed: " + msg.err.Error()))
		}
		m.header = msg.header
		m.promptTokens = 0 // gauge will update on the next model call
		return m, tea.Println(styleMeta.Render(fmt.Sprintf("switched to %s", msg.header.Model)))

	case doneMsg:
		m.busy = false
		m.thinking = false
		m.lastErr = msg.err
		out := m.tr.flush(m.width)               // a turn that errored never sent TurnFinished
		cmds := []tea.Cmd{waitForDone(m.b.done)} // re-issue THIS listener only
		if len(out) > 0 {
			cmds = append(cmds, tea.Println(strings.Join(out, "\n")))
		}
		// Print a fresh git summary so the user can see the workspace state after
		// the agent's changes without leaving the TUI.
		if gs := gitSummaryLine(); gs != "" {
			cmds = append(cmds, tea.Println(gs))
		}
		return m, tea.Batch(cmds...)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	// Forward any unhandled message (cursor.BlinkMsg etc.) to the composer so the
	// textarea's blink/cursor tracking stays alive — critical for IME positioning.
	if !m.busy {
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(msg)
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
	// A delegated subagent runs in its own session, so its events carry a different
	// SessionID. Route them to a condensed heartbeat in the status line — NEVER the
	// transcript, which would re-flood exactly what delegation keeps out.
	if ev.SessionID != "" && m.header.Session != "" && ev.SessionID != m.header.Session {
		return m.handleSubagentEvent(ev)
	}

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
	if len(out) > 0 {
		return m, tea.Batch(
			tea.Println(strings.Join(out, "\n")),
			waitForEvent(m.b.events),
		)
	}
	return m, waitForEvent(m.b.events)

}

// handleSubagentEvent folds a delegated subagent's event stream into the live
// status line — step count and current tool — and prints nothing to the
// transcript, so the parent's scrollback stays clean (default-quiet).
func (m model) handleSubagentEvent(ev agent.Event) (tea.Model, tea.Cmd) {
	switch ev.Kind {
	case agent.EventTaskStarted:
		m.subActive, m.subStep, m.subTool = true, 0, ""
	case agent.EventModelStarted:
		m.subStep++ // one model call == one loop iteration (the budgeted unit)
	case agent.EventToolStarted:
		m.subTool = ev.ToolName
	case agent.EventTaskFinished:
		m.subActive, m.subStep, m.subTool = false, 0, ""
	}
	return m, waitForEvent(m.b.events)
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
	m.lastEsc = time.Time{}
	b := m.b
	return m, func() tea.Msg { b.inputs <- input; return nil }
}

// handleApprovalKey drives the approval card: ↑/↓ switches between approve and
// deny, Enter confirms, Esc denies. Direct y/n keys still work.
func (m model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k", "ctrl+p":
		m.approveIdx = 0
	case "down", "j", "ctrl+n":
		m.approveIdx = 1
	case "v", "V":
		m.showPreview = !m.showPreview
	case "enter", "y", "Y":
		m.pending.reply <- true
		m.pending, m.approveIdx, m.showPreview = nil, 0, false
		return m, nil // listeners stay alive independently (approvalMsg already re-issued waitForApproval)
	case "n", "N", "esc":
		m.pending.reply <- false
		m.pending, m.approveIdx, m.showPreview = nil, 0, false
		return m, nil
	case "ctrl+c":
		m.pending.reply <- false
		m.pending, m.approveIdx, m.showPreview = nil, 0, false
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
	m.lastEsc = time.Time{}
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
	m.b.sessSwap <- sess // buffered (cap 1); the run loop applies it between turns
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
//func (m *model) syncComposer() {
//	n := clampInt(strings.Count(m.composer.Value(), "\n")+1, minComposerLines, maxComposerLines)
//	if n != m.composerHeight {
//		m.composerHeight = n
//		m.composer.SetHeight(n)
//	}
//}

func (m *model) syncComposer() {
	// 1. 获取当前输入框的实际可用文本宽度
	promptWidth := runewidth.StringWidth(composerPrompt)
	availableWidth := m.width - composerRightPadding - promptWidth
	if availableWidth < 10 { // 防御性代码，防止终端过窄导致除以0
		availableWidth = 40
	}
	// 2. 精准计算视觉总行数
	visualLines := 0
	// 按用户的硬换行 (\n) 切割
	lines := strings.Split(m.composer.Value(), "\n")
	for _, line := range lines {
		if line == "" {
			visualLines++
			continue
		}
		// 计算这一行文字的绝对显示宽度
		w := runewidth.StringWidth(line)
		// 向上取整计算折行数。例如可用宽度 40，字宽 41，则占 2 行
		chunks := (w + availableWidth - 1) / availableWidth
		if chunks == 0 {
			chunks = 1
		}
		visualLines += chunks
	}

	// 3. 限制在最小和最大行数之间
	targetHeight := clampInt(visualLines, minComposerLines, maxComposerLines)

	// 4. 当高度真正发生变化时，同步更新组件
	if targetHeight != m.composerHeight {
		m.composerHeight = targetHeight
		m.composer.SetHeight(targetHeight)
	}
}

func composerWidth(terminalWidth int) int {
	return clampInt(terminalWidth-composerRightPadding, 1, terminalWidth)
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

// Listener discipline (the architecture that makes events flow): the three
// channel listeners — waitForEvent / waitForApproval / waitForDone — are started
// once in Init and each runs in its own goroutine. A Cmd returned from Update is
// ADDED (a new goroutine), never replaces the others. So each message handler
// must re-issue ONLY its own listener (event→waitForEvent, etc.); re-issuing all
// three would duplicate the other two and leak goroutines. Slash commands and
// picker actions just print — they don't touch the listeners, which stay alive
// independently.

// --- /use model picker ---------------------------------------------------

// openUse opens the model picker (no arg) or switches model directly (with a
// name). Refuses mid-turn — the swap lands at a turn boundary via modelSwap.
func (m *model) openUse(args string) tea.Cmd {
	if m.busy {
		return tea.Println("finish the current turn before switching models")
	}
	if args != "" {
		return m.useModel(args)
	}
	if len(m.src.modelNames) == 0 {
		return tea.Println("no other configured models")
	}
	m.modelPick = &modelPicker{models: m.src.modelNames}
	return nil
}

func (m model) handleModelPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.modelPick
	switch msg.String() {
	case "up", "k", "ctrl+p":
		if p.idx > 0 {
			p.idx--
		}
	case "down", "j", "ctrl+n":
		if p.idx < len(p.models)-1 {
			p.idx++
		}
	case "enter":
		if len(p.models) == 0 {
			m.modelPick = nil
			return m, nil
		}
		return m, m.useModel(p.models[p.idx].name)
	case "esc":
		m.modelPick = nil
	}
	return m, nil
}

// useModel sends the model name to the run-loop goroutine (swapped between
// turns), then awaits the result — the same async-safety pattern as /resume.
func (m *model) useModel(name string) tea.Cmd {
	m.modelPick = nil
	if m.src.modelSwap == nil {
		return tea.Println("model switch not available")
	}
	m.b.modelSwap <- name // buffered; the run loop applies it between turns
	return waitForModelSwapResult(m.b.modelSwapResult)
}

// --- live region --------------------------------------------------------

func (m model) View() string {
	if !m.ready {
		return ""
	}
	lines := []string{}
	if m.showThinking && m.busy && m.tr.step.thinking != "" {
		header := "▾ " + fmtStepHeader(m.tr.step)
		lines = append(lines, styleThinking.Render(header))
		for _, ln := range wrapProse(m.tr.step.thinking, m.width-4) {
			lines = append(lines, "    "+styleBody.Render(ln))
		}
	}
	lines = append(lines, m.statusLine())
	switch {
	case m.pending != nil:
		lines = append(lines, renderApprovalCard(*m.pending, m.approveIdx, m.width)...)
		if m.showPreview {
			lines = append(lines, renderApprovalPreview(*m.pending, m.width)...)
		}
	case m.picker != nil:
		lines = append(lines, renderPicker(*m.picker, m.width)...)
	case m.modelPick != nil:
		lines = append(lines, renderModelPicker(*m.modelPick, m.width)...)
	case m.paletteActive():
		cmds := filterCommands(m.composer.Value())
		lines = append(lines, renderPalette(cmds, clampInt(m.cmdIdx, 0, len(cmds)-1), m.width)...)
	default:
		lines = append(lines, styleMeta.Render(m.hint()))
	}
	cv := m.composer.View()
	if l := len(cv); l > 0 && cv[l-1] == '\n' {
		cv = cv[:l-1]
	}
	lines = append(lines, cv)
	// BubbleTea inline mode positions the View one line above where it belongs,
	// overlaying scrollback. Three leading empty lines shift the live region down
	// so the clear-to-end-of-line + composer text don't bleed into the transcript.
	return "\n\n\n" + strings.Join(lines, "\n")
}

func (m model) composerCursorColumn() int {
	value := m.composer.Value()
	if i := strings.LastIndex(value, "\n"); i >= 0 {
		value = value[i+1:]
	}
	textWidth := runewidth.StringWidth(value)
	promptWidth := runewidth.StringWidth(composerPrompt)
	contentWidth := composerWidth(m.width) - promptWidth
	if contentWidth < 1 {
		contentWidth = 1
	}
	visualCol := textWidth
	if visualCol > contentWidth {
		visualCol %= contentWidth
		if visualCol == 0 {
			visualCol = contentWidth
		}
	}
	return clampInt(promptWidth+visualCol+1, 1, m.width)
}

// statusLine is the one live status row: what's happening on the left, the
// context gauge + skills on the right.
func (m model) statusLine() string {
	var left string
	switch {
	case m.subActive:
		s := fmt.Sprintf(" subagent · step %d", m.subStep)
		if m.header.SubagentBudget > 0 {
			s += fmt.Sprintf("/%d", m.header.SubagentBudget)
		}
		if m.subTool != "" {
			s += " · " + m.subTool
		}
		left = m.spinner.View() + styleMeta.Render(s)
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
	return "enter send · alt+enter newline · / commands · ctrl+o hide thinking · ctrl+z suspend (fg resumes) · ctrl+c quit"
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
