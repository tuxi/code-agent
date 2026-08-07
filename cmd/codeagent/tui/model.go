package tui

import (
	"code-agent/cmd/codeagent/tui/theme"
	"fmt"
	"strings"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/tools"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

// model is the BubbleTea program (alt-screen): a full-screen workspace renderer.
// Finalized events are appended to the internal scrollback buffer (m.buf) and
// shown through a viewport, since alt-screen owns the whole terminal; the live
// region it redraws each frame is the status line, any open overlay, and the
// composer at the very bottom. It renders the event stream and owns no control
// flow — the agent does.
type model struct {
	b      *Backend
	header HeaderInfo

	composer textarea.Model
	spinner  spinner.Model
	// tr renders the agent event stream into the printed transcript — shared with
	// /resume history replay (transcript.go).
	tr transcript

	// buf is the alt-screen scrollback. Inline mode printed finalized events to
	// the terminal's own scrollback via appendLines; alt-screen owns the whole
	// screen, so finalized events are appended here and rendered through vp.
	buf []string
	// vp is the scrollable transcript viewport (alt-screen replacement for the
	// terminal's native scrollback).
	vp viewport.Model

	showThinking bool             // ctrl+o toggle: show current step's thinking in the live region (on by default)
	planState    agent.PlanStatus // current plan state (synced from events + ctrl+p toggle)
	lastEsc      time.Time        // double-Esc clears the composer (like Claude Code)

	src     sessionSource  // saved-session / model access for slash commands
	overlay Overlay        // the open modal in the live region (approval card, picker…); nil = none
	palette paletteOverlay // the slash-command menu: always alive, shown when paletteActive()

	busy            bool             // a turn is running; submit is locked
	thinking        bool             // a model call is in flight; show the spinner
	lastErr         error

	promptTokens int             // latest prompt size (from EventModelFinished) for the gauge
	skills       map[string]bool // distinct skills loaded this session

	// Subagent heartbeat: a delegated `task` runs in its own session, so its events
	// arrive with a different SessionID. We surface them as a condensed status line
	// (never the transcript — that would re-flood what delegation keeps out).
	subActive bool
	subStep   int    // subagent loop iterations (EventModelStarted count)
	subTool   string // the tool the subagent's current iteration is running

	// todos is the model's current task checklist (8.4), shown as a live panel in
	// the live region and updated whole-list on each EventTodoUpdated.
	todos []tools.Todo

	// streaming is the live, ephemeral preview of the model's text as it generates
	// (8.6): EventTokenDelta appends; it is reset around each model call and never
	// enters the transcript (the finalized answer prints via EventTurnFinished).
	streaming string

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
	sp.Style = theme.Default.Skill

	return model{
		b:              b,
		header:         header,
		composer:       ta,
		spinner:        sp,
		skills:         map[string]bool{},
		src:            src,
		showThinking:   true, // on by default — thinking is the signal, ctrl+o hides it
		composerHeight: minComposerLines,
		palette:        paletteOverlay{},
		vp:             viewport.New(0, 0),
	}
}

// appendMsg carries pre-styled lines to be added to the scrollback buffer. In
// inline mode these were appendLines calls (the terminal owned the scrollback);
// in alt-screen mode the model owns the scrollback, so appending is a state
// mutation handled in Update, not a side-effecting Cmd.
type appendMsg []string

// appendLines is the drop-in replacement for appendLines: it returns a tea.Cmd
// that emits an appendMsg. Update handles it by extending m.buf and scrolling
// the viewport to the bottom.
func appendLines(lines ...string) tea.Cmd {
	msg := appendMsg(lines)
	return func() tea.Msg { return msg }
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		textarea.Blink,
		m.spinner.Tick,
		waitForEvent(m.b.events),
		waitForApproval(m.b.approvals),
		waitForPlanApproval(m.b.planApprovals),
		waitForAskUser(m.b.askUsers),
		waitForDone(m.b.done),
		waitForGoalDone(m.b.goalDone),
	}
	// Banner added to the scrollback buffer once at startup — no git summary
	// here (it follows each turn).
	return tea.Batch(append([]tea.Cmd{appendLines(m.banner())}, cmds...)...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.composer.SetWidth(composerWidth(msg.Width))
		m.ready = true
		m.syncComposer() // <- 宽度变了，折行数也会变，必须同步！
		// Size the viewport to the terminal height minus the header and bottom
		// region. The exact reserved height is computed in View(); here we give
		// it a reasonable default that View's layout will refine.
		m.vp.Width = msg.Width
		m.vp.Height = msg.Height - reservedHeight
		return m, nil

	case appendMsg:
		// Alt-screen scrollback: former appendLines output lands here instead.
		m.buf = append(m.buf, msg...)
		m.vp.SetContent(strings.Join(m.buf, "\n"))
		m.vp.GotoBottom()
		return m, nil

	case tea.KeyMsg:
		// An open overlay gets first crack at every key. The modal cards
		// (approval, plan, ask_user) consume everything — a tool or decision is
		// waiting. The pickers yield the global keys (ctrl+c/z/o/p) and consume
		// the rest, matching the old routing where the global switch ran before
		// the picker handlers.
		if m.overlay != nil {
			if next, handled, cmd := m.overlay.Key(msg, &m); handled {
				m.overlay = next
				return m, cmd
			}
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
		case "ctrl+p":
			// Toggle read-only plan mode. The run loop applies it at the next turn
			// boundary; the send is async so it never blocks the UI.
			if m.planState == agent.PlanStatusPlanning || m.planState == agent.PlanStatusProposing {
				m.planState = agent.PlanStatusNone
			} else {
				m.planState = agent.PlanStatusPlanning
			}
			desired := m.planState != agent.PlanStatusNone
			return m, func() tea.Msg { m.b.planToggle <- desired; return nil }
		}
		if m.paletteActive() {
			if _, handled, cmd := m.palette.Key(msg, &m); handled {
				return m, cmd
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

	case askUserMsg:
		req := askUserReq(msg)
		m.overlay = &askUserOverlay{q: req.q, reply: req.reply}
		return m, waitForAskUser(m.b.askUsers)

	case planApprovalMsg:
		req := planApprovalReq(msg)
		m.overlay = &planOverlay{plan: req.plan, reply: req.reply}
		return m, waitForPlanApproval(m.b.planApprovals)

	case eventMsg:
		return m.handleEvent(agent.Event(msg))

	case approvalMsg:
		req := approvalReq(msg)
		m.overlay = &approvalOverlay{req: req, granter: m.src.granter}
		return m, waitForApproval(m.b.approvals)

	case modelSwappedMsg:
		if msg.err != nil {
			return m, appendLines(theme.Default.Fail.Render("model switch failed: " + msg.err.Error()))
		}
		m.header = msg.header
		m.promptTokens = 0 // gauge will update on the next model call
		return m, appendLines(theme.Default.Meta.Render(fmt.Sprintf("switched to %s", msg.header.Model)))

	case doneMsg:
		m.busy = false
		m.thinking = false
		m.lastErr = msg.err
		out := m.tr.flush(m.width)               // a turn that errored never sent TurnFinished
		cmds := []tea.Cmd{waitForDone(m.b.done)} // re-issue THIS listener only
		if len(out) > 0 {
			cmds = append(cmds, appendLines(strings.Join(out, "\n")))
		}
		// Print a fresh git summary so the user can see the workspace state after
		// the agent's changes without leaving the TUI.
		if gs := gitSummaryLine(); gs != "" {
			cmds = append(cmds, appendLines(gs))
		}
		return m, tea.Batch(cmds...)

	case promptRenderedMsg:
		// The MCP prompt template rendered: on error, unlock and report; otherwise
		// feed the rendered text into the turn path (busy stays set through the turn).
		if msg.err != nil {
			m.busy = false
			return m, appendLines("error: " + msg.err.Error())
		}
		b := m.b
		return m, func() tea.Msg { b.inputs <- msg.text; return nil }

	case goalCtlResultMsg:
		return m, appendLines(string(msg))

	case goalDoneMsg:
		m.busy = false
		m.thinking = false
		m.lastErr = msg.err
		out := m.tr.flush(m.width) // surface any buffered transcript from the last turn
		cmds := []tea.Cmd{waitForGoalDone(m.b.goalDone)}
		if len(out) > 0 {
			cmds = append(cmds, appendLines(strings.Join(out, "\n")))
		}
		if msg.err != nil {
			cmds = append(cmds, appendLines(theme.Default.Fail.Render("goal: "+msg.err.Error())))
		} else if msg.summary != "" {
			cmds = append(cmds, appendLines(msg.summary))
		}
		if gs := gitSummaryLine(); gs != "" {
			cmds = append(cmds, appendLines(gs))
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

	// The checklist lives in the live region (a panel), never the scrollback — so
	// updating it is just live state, with no transcript output.
	if ev.Kind == agent.EventTodoUpdated {
		m.todos = ev.Todos
		return m, waitForEvent(m.b.events)
	}

	// Plan-mode state is live UI state (the ⏸ PLAN badge), never transcript
	// output — same treatment as the checklist. This is what makes the badge
	// react to the model's own enter_plan_mode, not just the user's ctrl+p.
	if ev.Kind == agent.EventPlanStateChanged {
		m.planState = ev.PlanState
		return m, waitForEvent(m.b.events)
	}

	// Streamed text (8.6): an ephemeral live preview, never the transcript. It is
	// cleared around each model call (below), so the authoritative render — the
	// step card or the final reply printed to scrollback — takes over cleanly.
	if ev.Kind == agent.EventTokenDelta {
		m.streaming += ev.Text
		return m, waitForEvent(m.b.events)
	}
	// Reasoning deltas update only the current step's live expandable body. The
	// persisted EventThinking snapshot replaces this buffer when the call ends.
	if ev.Kind == agent.EventReasoningDelta {
		m.tr.step.thinking += ev.Text
		return m, waitForEvent(m.b.events)
	}

	// Live UI state (spinner, gauge, skills) — separate from transcript rendering.
	switch ev.Kind {
	case agent.EventModelStarted:
		m.thinking = true
		m.streaming = ""
	case agent.EventModelFinished:
		m.thinking = false
		m.streaming = ""
		if ev.PromptTokens > 0 {
			m.promptTokens = ev.PromptTokens
		}
	case agent.EventSkillLoaded:
		m.skills[ev.ToolName] = true
	}

	out := m.tr.render(ev, m.width)
	if len(out) > 0 {
		str := strings.Join(out, "\n")
		return m, tea.Batch(
			appendLines(str),
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

	// An MCP prompt (/mcp__server__prompt): render the server's template to text
	// off the UI goroutine (it is a network call), then run that text as a turn via
	// the promptRenderedMsg handler. Everything else runs the typed line directly.
	if strings.HasPrefix(input, "/mcp__") {
		if m.src.promptOps == nil {
			m.busy = false
			return m, appendLines("MCP prompts are not available")
		}
		fields := strings.Fields(input)
		command := strings.TrimPrefix(fields[0], "/")
		args := fields[1:]
		po := m.src.promptOps
		return m, func() tea.Msg {
			text, err := po.Render(command, args)
			return promptRenderedMsg{text: text, err: err}
		}
	}

	b := m.b
	return m, func() tea.Msg { b.inputs <- input; return nil }
}

// --- command palette ----------------------------------------------------

// paletteActive reports whether the slash-command menu should show: no overlay
// open, and the line so far matches at least one command.
func (m model) paletteActive() bool {
	return m.overlay == nil && len(filterCommands(m.composer.Value())) > 0
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

// reservedHeight is the terminal rows reserved for the header and bottom
// region (status + overlays + composer). The viewport fills the rest. This is
// a conservative lower bound; the bottom region can be taller when overlays
// are open, in which case the viewport content is simply clipped at the top
// (viewport scrolls). A precise per-frame height is a later refinement.
const reservedHeight = 6

// View renders the full-screen alt-screen layout: a fixed header, the
// scrollable transcript viewport, and a bottom region (live status + overlays
// + composer). Formerly inline mode printed finalized events to the
// terminal's own scrollback via appendLines; now they live in m.buf and are
// rendered through the viewport.
func (m model) View() string {
	if !m.ready {
		return ""
	}
	// Header (fixed top).
	header := m.bannerLine()

	// Transcript viewport (fills the middle).
	m.vp.SetContent(strings.Join(m.buf, "\n"))
	transcript := m.vp.View()

	// Bottom region: live preview + todos + status + overlays + composer.
	bottom := m.renderBottom()

	return lipgloss.JoinVertical(lipgloss.Left, header, transcript, bottom)
}

// bannerLine is the one-line header: CodeAgent · model · workspace.
func (m model) bannerLine() string {
	parts := []string{"CodeAgent"}
	if m.header.Model != "" {
		parts = append(parts, m.header.Model)
	}
	if m.header.Workspace != "" {
		parts = append(parts, m.header.Workspace)
	}
	return theme.Default.AsstLabel.Render(strings.Join(parts, " · "))
}

// renderBottom is the live region below the viewport: streaming/thinking
// preview, todo panel, status line, overlay cards, and the composer.
func (m model) renderBottom() string {
	lines := []string{}
	switch {
	case m.streaming != "":
		// Streamed text typing out live (8.6) — takes the live region while a call
		// is in flight; replaced by the step card / final reply once it resolves.
		for _, ln := range wrapProse(m.streaming, m.width-2) {
			lines = append(lines, theme.Default.Body.Render(ln))
		}
	case m.showThinking && m.busy && m.tr.step.thinking != "":
		header := "▾ " + fmtStepHeader(m.tr.step)
		lines = append(lines, theme.Default.Thinking.Render(header))
		for _, ln := range wrapProse(m.tr.step.thinking, m.width-4) {
			lines = append(lines, "    "+theme.Default.Body.Render(ln))
		}
	}
	lines = append(lines, m.todoPanel()...)
	lines = append(lines, m.statusLine())
	switch {
	case m.overlay != nil:
		lines = append(lines, m.overlay.View(m.width, &m)...)
	case m.paletteActive():
		lines = append(lines, m.palette.View(m.width, &m)...)
	default:
		lines = append(lines, theme.Default.Meta.Render(m.hint()))
	}
	cv := m.composer.View()
	if l := len(cv); l > 0 && cv[l-1] == '\n' {
		cv = cv[:l-1]
	}
	lines = append(lines, cv)
	return strings.Join(lines, "\n")
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
		left = m.spinner.View() + theme.Default.Meta.Render(s)
	case m.thinking:
		left = m.spinner.View() + theme.Default.Meta.Render(" thinking…")
	case m.busy:
		left = theme.Default.Meta.Render("working…")
	case m.lastErr != nil:
		left = theme.Default.Fail.Render("error: " + m.lastErr.Error())
	default:
		left = theme.Default.Meta.Render("ready")
	}
	if m.planState != agent.PlanStatusNone {
		left = theme.Default.Skill.Render("⏸ PLAN") + "  " + left
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
	return left + "   " + theme.Default.Meta.Render(strings.Join(right, " · "))
}

// todoPanel renders the model's checklist as a compact live panel (8.4): a header
// with the done/total count plus one line per item. The in-progress item is
// highlighted and shows its present-tense activeForm; completed items are dimmed.
func (m model) todoPanel() []string {
	if len(m.todos) == 0 {
		return nil
	}
	done := 0
	for _, td := range m.todos {
		if td.Status == tools.TodoCompleted {
			done++
		}
	}
	out := []string{theme.Default.Meta.Render(fmt.Sprintf("Todos %d/%d", done, len(m.todos)))}
	for _, td := range m.todos {
		out = append(out, "  "+todoLine(td))
	}
	return out
}

func todoLine(td tools.Todo) string {
	switch td.Status {
	case tools.TodoCompleted:
		return theme.Default.Meta.Render("☑ " + td.Content)
	case tools.TodoInProgress:
		label := td.Content
		if td.ActiveForm != "" {
			label = td.ActiveForm
		}
		return theme.Default.Skill.Render("▶ " + label)
	default:
		return theme.Default.Body.Render("☐ " + td.Content)
	}
}

func (m model) hint() string {
	return "enter send · alt+enter newline · / commands · ctrl+p plan · ctrl+o hide thinking · ctrl+z suspend (fg resumes) · ctrl+c quit"
}

func (m model) banner() string {
	parts := []string{"CodeAgent"}
	if m.header.Model != "" {
		parts = append(parts, m.header.Model)
	}
	if m.header.Workspace != "" {
		parts = append(parts, m.header.Workspace)
	}
	line := theme.Default.AsstLabel.Render(strings.Join(parts, " · "))
	return line + "\n" + theme.Default.Meta.Render("type a request, or /help for commands")
}
