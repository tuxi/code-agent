// Package-level note: this file is the top-level BubbleTea model of the TUI
// (the "app"). It owns key routing (§2 of the port plan), the split layout
// (chat page + status bar), the modal dialog overlays, and the dispatch of the
// runner's channels (backend.go) into the chat transcript (conversation.go) and
// the approval/plan/askuser dialogs.
//
// Signature contract with run.go: newModel(b *Backend, header HeaderInfo,
// src sessionSource) tea.Model. All waitFor* listeners are re-issued by their
// own handler after each delivery (the listener-discipline from the old model).
//
// NOTE for the commands.go rewire (separate task): this file now owns the
// slash-command methods runCommand/useModel/resume/goalDispatch/startPursuit/
// goalCtl/toggleAuto/sessions/promptHelp plus the model type itself. The old
// commands.go copies of these (which reference the deleted m.composer/m.palette
// fields) must be dropped; only the pure helpers (commandToken, commandArgs,
// onOff, filterCommands, lookupCommand) survive. The old model.go is deleted
// wholesale (it defines a conflicting `type model struct` and `newModel`).
package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"code-agent/cmd/codeagent/tui/components/chat"
	"code-agent/cmd/codeagent/tui/components/core"
	"code-agent/cmd/codeagent/tui/components/dialog"
	"code-agent/cmd/codeagent/tui/layout"
	"code-agent/cmd/codeagent/tui/page"
	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/util"
	"code-agent/internal/agent"
)

// keyMap is the app-level key routing (§2). Composer keys (enter to send,
// arrows to edit, pgup/pgdn to scroll) belong to the chat page / editor / list
// and are only reachable when no dialog is open.
type keyMap struct {
	Command  key.Binding // ctrl+k: command dialog
	Model    key.Binding // ctrl+o: model dialog
	Theme    key.Binding // ctrl+t: theme dialog
	Sessions key.Binding // ctrl+s: session dialog
	Help     key.Binding // ctrl+?: help overlay
	Quit     key.Binding // ctrl+c: quit confirm
	Cancel   key.Binding // esc: close dialog / cancel (busy → CancelTurn)
}

func defaultKeys() keyMap {
	return keyMap{
		Command:  key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("ctrl+k", "commands")),
		Model:    key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("ctrl+o", "switch model")),
		Theme:    key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "theme")),
		Sessions: key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "sessions")),
		Help:     key.NewBinding(key.WithKeys("ctrl+?"), key.WithHelp("ctrl+?", "help")),
		Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
		Cancel:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel/close")),
	}
}

// model is the top-level TUI model: a chat page with a status bar, plus modal
// dialogs rendered as overlays. It renders the event stream and owns no control
// flow — the agent does (events and approvals cross over the Backend channels).
type model struct {
	b      *Backend
	header HeaderInfo
	src    sessionSource

	width  int
	height int

	layout layout.SplitPaneLayout
	status core.StatusCmp

	// conversation folds the agent event stream into chat.Message transcript
	// entries; on /resume it is rebuilt from the session's persisted events.
	conversation *Conversation

	busy bool // a turn or /goal pursuit is running; the composer is locked

	// dialog is the active modal overlay; nil when none. blocking marks the
	// runner-waiting dialogs (permission / plan / askuser) whose esc/ctrl+c
	// answer the request instead of merely closing the overlay.
	dialog   tea.Model
	blocking bool

	permission   dialog.PermissionDialogCmp
	planApproval dialog.PlanApprovalDialogCmp
	askUser      dialog.AskUserDialogCmp
	command      dialog.CommandDialog
	modelDlg     dialog.ModelDialog
	sessionDlg   dialog.SessionDialog
	themeDlg     dialog.ThemeDialog
	help         dialog.HelpCmp
	quit         dialog.QuitDialog

	keys keyMap

	sysSeq int // backs stable IDs for app-generated system messages
}

// newModel builds the app. run.go calls it once per program; the signature is
// the contract with run.go.
func newModel(b *Backend, header HeaderInfo, src sessionSource) tea.Model {
	chatPage := page.NewChatPage()
	status := core.NewStatusCmp()
	status.SetModel(header.Model)
	status.SetGit(gitStatus()) // one-shot at startup; a future turn can refresh

	m := &model{
		b:      b,
		header: header,
		src:    src,

		status: status,
		layout: layout.NewSplitPane(
			layout.WithLeftPanel(chatPage),
			layout.WithBottomPanel(layout.NewContainer(status)),
		),

		conversation: &Conversation{},

		permission:   dialog.NewPermissionDialogCmp(src.granter),
		planApproval: dialog.NewPlanApprovalDialogCmp(),
		askUser:      dialog.NewAskUserDialogCmp(),
		command:      dialog.NewCommandDialogCmp(),
		modelDlg:     dialog.NewModelDialogCmp(modelOptions(src.modelNames)),
		sessionDlg:   dialog.NewSessionDialogCmp(),
		themeDlg:     dialog.NewThemeDialogCmp(),
		help:         dialog.NewHelpCmp(),
		quit:         dialog.NewQuitCmp(),

		keys: defaultKeys(),
	}
	m.help.SetBindings(layout.KeyMapToSlice(m.keys))
	chatPage.SetOnSubmit(func(text string) tea.Cmd { return m.submit(text) })
	m.command.SetCommands(m.commandList())
	return m
}

// modelOptions maps the run loop's model list into the picker's options.
func modelOptions(infos []modelInfo) []dialog.ModelOption {
	opts := make([]dialog.ModelOption, 0, len(infos))
	for _, mi := range infos {
		opts = append(opts, dialog.ModelOption{Name: mi.name, Display: mi.display})
	}
	return opts
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		m.layout.Init(),
		waitForEvent(m.b.events),
		waitForDone(m.b.done),
		waitForApproval(m.b.approvals),
		waitForPlanApproval(m.b.planApprovals),
		waitForAskUser(m.b.askUsers),
		waitForGoalDone(m.b.goalDone),
	)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.updateKey(msg)
	case tea.WindowSizeMsg:
		return m.updateWindowSize(msg)

	// --- runner channel dispatches ----------------------------------------
	case eventMsg:
		return m, m.handleEvent(agent.Event(msg))
	case approvalMsg:
		req := approvalReq(msg)
		m.dialog = m.permission
		m.blocking = true
		return m, tea.Batch(
			m.permission.SetPermissions(dialog.ApprovalRequest{Tool: req.tool, Input: req.input, Reply: req.reply}),
			waitForApproval(m.b.approvals),
		)
	case planApprovalMsg:
		req := planApprovalReq(msg)
		m.dialog = m.planApproval
		m.blocking = true
		return m, tea.Batch(
			m.planApproval.SetPlan(dialog.PlanApprovalRequest{Plan: req.plan, Reply: req.reply}),
			waitForPlanApproval(m.b.planApprovals),
		)
	case askUserMsg:
		req := askUserReq(msg)
		m.dialog = m.askUser
		m.blocking = true
		return m, tea.Batch(
			m.askUser.SetQuestion(dialog.AskUserRequest{Question: req.q, Reply: req.reply}),
			waitForAskUser(m.b.askUsers),
		)
	case doneMsg:
		// The single source of "the composer is free again" (robust to the
		// error path, where no EventTurnFinished is emitted).
		m.busy = false
		m.status.SetBusy(false)
		if msg.err != nil {
			cmds := []tea.Cmd{waitForDone(m.b.done), util.ReportError(msg.err)}
			// No EventTurnFinished on the error path: finalize any in-flight
			// streaming block so it stops spinning.
			if msgs := m.conversation.finalizeStreaming(); len(msgs) > 0 {
				cmds = append(cmds, util.CmdHandler(chat.BatchMessagesMsg{Messages: msgs}))
			}
			return m, tea.Batch(cmds...)
		}
		return m, waitForDone(m.b.done)
	case goalDoneMsg:
		m.busy = false
		m.status.SetBusy(false)
		if msg.err != nil {
			return m, tea.Batch(m.system("✕ goal failed: "+msg.err.Error()), util.ReportError(msg.err))
		}
		return m, m.system("🎯 goal: " + msg.summary)
	case goalCtlResultMsg:
		return m, m.system("↻ " + string(msg))
	case modelSwappedMsg:
		if msg.err != nil {
			return m, util.ReportError(msg.err)
		}
		m.header = msg.header
		m.status.SetModel(msg.header.Model)
		return m, m.system("↻ model: " + msg.header.Model)
	case promptRenderedMsg:
		if msg.err != nil {
			return m, util.ReportError(msg.err)
		}
		if strings.TrimSpace(msg.text) == "" {
			return m, nil
		}
		return m, m.startTurn(msg.text)

	// --- dialog outcomes ---------------------------------------------------
	case dialog.PermissionResponseMsg,
		dialog.ClosePlanApprovalMsg,
		dialog.CloseAskUserMsg,
		dialog.CloseCommandDialogMsg,
		dialog.CloseModelDialogMsg,
		dialog.CloseSessionDialogMsg,
		dialog.CloseThemeDialogMsg,
		dialog.CloseQuitMsg:
		return m, m.closeDialog()
	case dialog.ModelSelectedMsg:
		return m, tea.Batch(m.closeDialog(), m.useModel(msg.Name))
	case dialog.SessionSelectedMsg:
		return m, tea.Batch(m.closeDialog(), m.resume(msg.ID))
	case dialog.ThemeChangedMsg:
		// The dialog already applied theme.SetTheme; the layout rerenders on
		// this message (List drops its cache, editor re-creates its textarea).
		styles.ClearMarkdownRendererCache()
		u, cmd := m.layout.Update(msg)
		m.layout = u.(layout.SplitPaneLayout)
		return m, tea.Batch(cmd, m.closeDialog())
	case dialog.CommandSelectedMsg:
		m.dialog = nil
		m.blocking = false
		if msg.Command.Handler != nil {
			return m, msg.Command.Handler(msg.Command)
		}
		return m, nil

	case tea.MouseMsg:
		// An open overlay owns the pointer: swallowing mouse keeps stray clicks
		// under a dialog from toggling transcript folds behind it.
		if m.dialog != nil {
			return m, nil
		}
		u, cmd := m.layout.Update(msg)
		m.layout = u.(layout.SplitPaneLayout)
		return m, cmd
	}

	// Default: everything else goes to the layout (chat page / list / editor /
	// status bar) — ThemeChangedMsg rerenders, spinner ticks, viewport msgs.
	u, cmd := m.layout.Update(msg)
	m.layout = u.(layout.SplitPaneLayout)
	return m, cmd
}

// updateKey routes one key press. An open dialog owns the keyboard; otherwise
// the app-level keys are checked and everything else reaches the layout.
func (m *model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.dialog != nil {
		// Blocking dialogs (permission/plan/askuser) answer the runner request
		// on esc/ctrl+c themselves — forward everything. Non-blocking dialogs
		// are closed by esc at the app level.
		if key.Matches(msg, m.keys.Cancel) && !m.blocking {
			return m, m.closeDialog()
		}
		return m.forwardToDialog(msg)
	}
	switch {
	case key.Matches(msg, m.keys.Command):
		return m, m.openCommandDialog()
	case key.Matches(msg, m.keys.Model):
		return m, m.openModelDialog()
	case key.Matches(msg, m.keys.Theme):
		return m, m.openThemeDialog()
	case key.Matches(msg, m.keys.Sessions):
		return m, m.openSessionDialog()
	case key.Matches(msg, m.keys.Help):
		return m, m.openHelp()
	case key.Matches(msg, m.keys.Quit):
		return m, m.openQuit()
	case key.Matches(msg, m.keys.Cancel):
		// esc: cancel an in-flight turn; otherwise a no-op (the chat page has
		// its own informational cancel for the composer).
		if m.busy {
			m.b.CancelTurn()
		}
		return m, nil
	}
	u, cmd := m.layout.Update(msg)
	m.layout = u.(layout.SplitPaneLayout)
	return m, cmd
}

// forwardToDialog sends a key press to the active dialog and stores its updated
// model back into the field, so the dialog's own answer/close messages flow.
//
// Dispatch is by POINTER IDENTITY against the known dialog fields, not by a
// type switch on the interfaces: several dialog interfaces (ThemeDialog,
// ModelDialog, QuitDialog) share the identical method set (tea.Model +
// layout.Bindings), so a type switch matches the first listed case and
// mis-routes every dialog that satisfies several of them — e.g. the quit
// dialog matched ModelDialog (and would match ThemeDialog) and swallowed its
// answer. Pointer identity is unambiguous.
func (m *model) forwardToDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var u tea.Model
	switch {
	case m.dialog == m.permission:
		u, cmd = m.permission.Update(msg)
		m.permission = u.(dialog.PermissionDialogCmp)
	case m.dialog == m.planApproval:
		u, cmd = m.planApproval.Update(msg)
		m.planApproval = u.(dialog.PlanApprovalDialogCmp)
	case m.dialog == m.askUser:
		u, cmd = m.askUser.Update(msg)
		m.askUser = u.(dialog.AskUserDialogCmp)
	case m.dialog == m.command:
		u, cmd = m.command.Update(msg)
		m.command = u.(dialog.CommandDialog)
	case m.dialog == m.modelDlg:
		u, cmd = m.modelDlg.Update(msg)
		m.modelDlg = u.(dialog.ModelDialog)
	case m.dialog == m.sessionDlg:
		u, cmd = m.sessionDlg.Update(msg)
		m.sessionDlg = u.(dialog.SessionDialog)
	case m.dialog == m.themeDlg:
		u, cmd = m.themeDlg.Update(msg)
		m.themeDlg = u.(dialog.ThemeDialog)
	case m.dialog == m.help:
		u, cmd = m.help.Update(msg)
		m.help = u.(dialog.HelpCmp)
	case m.dialog == m.quit:
		u, cmd = m.quit.Update(msg)
		m.quit = u.(dialog.QuitDialog)
	default:
		return m, nil
	}
	m.dialog = u
	return m, cmd
}

// updateWindowSize tracks the terminal size and forwards the message to the
// layout and to every dialog (the permission/plan/askuser dialogs size
// themselves from their stored window size when a request arrives).
func (m *model) updateWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width, m.height = msg.Width, msg.Height
	var cmds []tea.Cmd
	u, cmd := m.layout.Update(msg)
	m.layout = u.(layout.SplitPaneLayout)
	cmds = append(cmds, cmd)
	for _, d := range []tea.Model{
		m.permission, m.planApproval, m.askUser,
		m.command, m.modelDlg, m.sessionDlg, m.themeDlg,
		m.help, m.quit,
	} {
		if d == nil {
			continue
		}
		_, cmd := d.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// handleEvent folds one agent event into the conversation and dispatches the
// resulting transcript messages to the chat List. The event listener is
// re-issued after every event.
func (m *model) handleEvent(ev agent.Event) tea.Cmd {
	var cmds []tea.Cmd
	switch ev.Kind {
	case agent.EventModelStarted:
		m.status.SetBusy(true)
	case agent.EventModelFinished:
		// Context gauge: the last invocation's prompt size, compared against
		// header.CompactThreshold by the user.
		if ev.PromptTokens > 0 {
			m.status.SetTokens(int64(ev.PromptTokens))
		}
	}
	// One event's messages travel as a single ordered batch. The old path sent
	// each message as its own command via tea.Batch, which runs commands
	// concurrently with no delivery-order guarantees — the List could receive
	// messages from one event in any order. A single BatchMessagesMsg applies
	// them in Apply order by construction.
	if msgs := m.conversation.Apply(ev); len(msgs) > 0 {
		cmds = append(cmds, util.CmdHandler(chat.BatchMessagesMsg{Messages: msgs}))
	}
	cmds = append(cmds, waitForEvent(m.b.events))
	return tea.Batch(cmds...)
}

// --- dialog openers ----------------------------------------------------------

func (m *model) openCommandDialog() tea.Cmd {
	m.command.SetCommands(m.commandList())
	m.dialog = m.command
	m.blocking = false
	return m.command.Init()
}

func (m *model) openModelDialog() tea.Cmd {
	m.dialog = m.modelDlg
	m.blocking = false
	return m.modelDlg.Init()
}

func (m *model) openThemeDialog() tea.Cmd {
	m.dialog = m.themeDlg
	m.blocking = false
	return m.themeDlg.Init()
}

func (m *model) openSessionDialog() tea.Cmd {
	m.sessionDlg.SetSessions(m.src.list())
	m.sessionDlg.SetSelectedSession("")
	m.dialog = m.sessionDlg
	m.blocking = false
	return m.sessionDlg.Init()
}

func (m *model) openHelp() tea.Cmd {
	m.dialog = m.help
	m.blocking = false
	return m.help.Init()
}

func (m *model) openQuit() tea.Cmd {
	m.dialog = m.quit
	m.blocking = false
	return m.quit.Init()
}

func (m *model) closeDialog() tea.Cmd {
	m.dialog = nil
	m.blocking = false
	return nil
}

// --- commands (also reachable through the composer text path) ----------------

// commandList is the ctrl+k command dialog's contents. Selecting one runs its
// handler; each handler is the same action the corresponding slash command
// takes from the composer.
func (m *model) commandList() []dialog.Command {
	return []dialog.Command{
		{ID: "help", Title: "/help", Description: "keyboard shortcuts", Handler: func(dialog.Command) tea.Cmd { return m.openHelp() }},
		{ID: "use", Title: "/use", Description: "switch model", Handler: func(dialog.Command) tea.Cmd { return m.openModelDialog() }},
		{ID: "resume", Title: "/resume", Description: "switch session", Handler: func(dialog.Command) tea.Cmd { return m.openSessionDialog() }},
		{ID: "goal", Title: "/goal", Description: "run a goal", Handler: func(dialog.Command) tea.Cmd { return m.goalDispatch("") }},
		{ID: "auto", Title: "/auto", Description: "toggle auto-approval", Handler: func(dialog.Command) tea.Cmd { return m.toggleAuto() }},
		{ID: "sessions", Title: "/sessions", Description: "list saved sessions", Handler: func(dialog.Command) tea.Cmd { return m.sessions() }},
		{ID: "prompts", Title: "/prompts", Description: "list MCP prompts", Handler: func(dialog.Command) tea.Cmd { return m.promptHelp() }},
	}
}

// submit is the chat page's onSubmit: composer enter lands here (the editor
// clears itself and emits chat.SendMsg, which the page forwards here).
func (m *model) submit(text string) tea.Cmd {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "/") {
		// MCP prompt invocation is async — the render runs off the UI goroutine
		// and comes back as promptRenderedMsg.
		if strings.HasPrefix(text, "/mcp__") && m.src.promptOps != nil {
			fields := strings.Fields(strings.TrimPrefix(text, "/"))
			if len(fields) == 0 {
				return nil
			}
			name, args := fields[0], fields[1:]
			return func() tea.Msg {
				rendered, err := m.src.promptOps.Render(name, args)
				return promptRenderedMsg{text: rendered, err: err}
			}
		}
		parts := strings.Fields(text)
		name := strings.TrimPrefix(parts[0], "/")
		args := strings.TrimSpace(strings.TrimPrefix(text, parts[0]))
		return m.runCommand(name, args)
	}
	return m.startTurn(text)
}

// startTurn queues a prompt for the runner and frees the composer on doneMsg.
func (m *model) startTurn(text string) tea.Cmd {
	if m.busy {
		return util.ReportWarn("busy — wait for the current turn to finish")
	}
	m.busy = true
	m.status.SetBusy(true)
	m.b.inputs <- text
	return waitForDone(m.b.done)
}

// runCommand dispatches a slash command; the dialog command handlers and the
// composer text path both land here.
func (m *model) runCommand(name, args string) tea.Cmd {
	switch name {
	case "help":
		return m.openHelp()
	case "use":
		if args == "" {
			return m.openModelDialog()
		}
		return m.useModel(args)
	case "resume":
		if args == "" {
			return m.openSessionDialog()
		}
		return m.resume(args)
	case "goal":
		return m.goalDispatch(args)
	case "auto":
		return m.toggleAuto()
	case "sessions":
		return m.sessions()
	case "prompts":
		return m.promptHelp()
	case "clear":
		return m.system("⤳ this build has no /clear — start fresh with /resume (ctrl+s)")
	case "new":
		return m.system("⤳ sessions are switched with /resume (ctrl+s)")
	default:
		return m.system("unknown command: /" + name + " (ctrl+? for help)")
	}
}

// useModel posts a model name to the run loop; the swap happens at a turn
// boundary and the result comes back as modelSwappedMsg.
func (m *model) useModel(name string) tea.Cmd {
	m.b.modelSwap <- name
	return waitForModelSwapResult(m.b.modelSwapResult)
}

// resume loads a saved session, swaps it into the run loop, and replays its
// persisted events into a fresh conversation.
func (m *model) resume(id string) tea.Cmd {
	sess, err := m.src.resume(id)
	if err != nil {
		return util.ReportError(err)
	}
	m.conversation = &Conversation{}
	m.busy = false
	m.status.SetBusy(false)

	// Fold every persisted event in order into a fresh conversation, then
	// deliver the whole transcript as ONE message. The old path sent each
	// message as its own command through tea.Batch, which runs commands
	// concurrently with no ordering guarantees — the displayed transcript
	// ended up scrambled (a later turn's prompt could land above an earlier
	// turn's reply). A single SetMessagesMsg rebuilds the List in slice order.
	var msgs []chat.Message
	for _, ev := range m.src.events(id) {
		msgs = append(msgs, m.conversation.Apply(ev)...)
	}
	swap := func() tea.Msg {
		// Swap the session into the run loop and deliver the full transcript
		// as ONE ordered message. Live events from the swapped session are
		// folded into this same conversation before the snapshot, so they are
		// included in the delivered transcript — nothing can scramble the
		// display order, because the transcript is a single ordered snapshot.
		m.b.sessSwap <- sess
		return chat.SetMessagesMsg{Messages: msgs}
	}
	return swap
}

func (m *model) goalDispatch(args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "", "status":
		return m.goalCtl(ctlStatus)
	case "clear":
		return m.goalCtl(ctlClear)
	default:
		return m.startPursuit(strings.TrimSpace(args))
	}
}

// startPursuit begins (or resumes, with "") a /goal pursuit. It runs like a
// long turn: events and approval cards flow through the same channels, and
// goalDoneMsg releases the composer.
func (m *model) startPursuit(objective string) tea.Cmd {
	if m.busy {
		return util.ReportWarn("busy — /goal runs between turns")
	}
	m.busy = true
	m.status.SetBusy(true)
	m.b.goalStart <- objective
	return waitForGoalDone(m.b.goalDone)
}

// goalCtl sends a quick between-turns goal op (status/clear) and waits for its
// one-line reply.
func (m *model) goalCtl(kind int) tea.Cmd {
	if m.busy {
		return util.ReportWarn("busy — /goal runs between turns")
	}
	reply := make(chan string, 1)
	m.b.goalCtl <- goalCtlReq{kind: kind, reply: reply}
	return func() tea.Msg { return goalCtlResultMsg(<-reply) }
}

func (m *model) toggleAuto() tea.Cmd {
	if m.src.auto == nil {
		return m.system("/auto is not available in this session")
	}
	enabled := !m.src.auto.Enabled()
	m.src.auto.SetEnabled(enabled)
	if enabled {
		return m.system("⚡ auto-approval: on")
	}
	return m.system("⚡ auto-approval: off")
}

func (m *model) sessions() tea.Cmd {
	metas := m.src.list()
	if len(metas) == 0 {
		return m.system("no saved sessions")
	}
	var b strings.Builder
	for _, meta := range metas {
		label := meta.Name
		if label == "" {
			label = meta.Title
		}
		fmt.Fprintf(&b, "• %s  %s  — %d msgs\n", meta.ID, label, meta.MessageCount)
	}
	return m.system(strings.TrimRight(b.String(), "\n"))
}

func (m *model) promptHelp() tea.Cmd {
	if m.src.promptOps == nil {
		return m.system("/prompts is not available in this session")
	}
	return m.system(m.src.promptOps.Help())
}

// system appends an app-generated notice (goal outcomes, command results) to
// the transcript. It bypasses the Conversation fold; the List upserts on ID, so
// a stable unique ID keeps these from colliding with event-folded messages.
func (m *model) system(content string) tea.Cmd {
	m.sysSeq++
	return util.CmdHandler(chat.NewMessageMsg{Message: chat.Message{
		ID:      fmt.Sprintf("sys%d", m.sysSeq),
		Kind:    chat.KindSystem,
		Content: content,
	}})
}

// View renders the layout, then places the active dialog on top, centered,
// with a drop shadow (the same overlay technique as the permission dialog).
func (m *model) View() tea.View {
	base := m.layout.View()
	if m.dialog == nil {
		base.AltScreen = true
		base.MouseMode = tea.MouseModeCellMotion
		return base
	}
	content := m.dialog.View().Content
	dw, dh := lipgloss.Width(content), lipgloss.Height(content)
	x := util.Clamp((m.width-dw)/2, 0, max(0, m.width-dw))
	y := util.Clamp((m.height-dh)/2, 0, max(0, m.height-dh))
	v := layout.PlaceOverlay(x, y, content, base.Content, true)
	vw := tea.NewView(v)
	vw.AltScreen = true
	vw.MouseMode = tea.MouseModeCellMotion
	return vw
}
