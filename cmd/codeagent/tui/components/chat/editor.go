// Package chat implements the chat transcript and composer components of the
// TUI, ported from opencode (internal/tui/components/chat) and adapted to
// code-agent's local infrastructure.
//
// Editor is the message composer: Enter sends, a trailing backslash + Enter
// inserts a newline, Ctrl+E opens $EDITOR (or nvim) for the message. It embeds
// the IME-compatible textarea sizing logic from the legacy model.go (see
// syncComposer): the composer starts one line tall so the cursor sits on the
// terminal's bottom row and the IME candidate window has room below it, and
// auto-grows up to maxComposerLines as content wraps. Mouse input is disabled
// (§8.9 no mouse): the whole TUI runs without a mouse driver.
package chat

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"code-agent/cmd/codeagent/tui/components/dialog"
	"code-agent/cmd/codeagent/tui/layout"
	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/cmd/codeagent/tui/util"
)

// Composer sizing constants — IME-friendly: a one-line composer keeps the
// cursor on the bottom row where the IME candidate window has room below it.
const (
	minComposerLines = 1
	maxComposerLines = 8

	composerPrompt       = "> "
	composerRightPadding = 1
)

// EditorKeyMaps are the composer bindings.
type EditorKeyMaps struct {
	Send             key.Binding
	OpenEditor       key.Binding
	InsertNewline    key.Binding
	BackslashNewline key.Binding // help-only: \ + Enter works on any terminal
	Escape           key.Binding // close the slash-command menu
}

var editorMaps = EditorKeyMaps{
	Send: key.NewBinding(
		key.WithKeys("enter", "ctrl+s"),
		key.WithHelp("enter", "send message"),
	),
	OpenEditor: key.NewBinding(
		key.WithKeys("ctrl+e"),
		key.WithHelp("ctrl+e", "open editor"),
	),
	InsertNewline: key.NewBinding(
		key.WithKeys("shift+enter"),
		key.WithHelp("shift+enter", "insert newline"),
	),
	BackslashNewline: key.NewBinding(
		key.WithKeys("\\+enter"),
		key.WithHelp("\\+enter", "insert newline (any terminal)"),
	),
	Escape: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "close menu"),
	),
}

// Editor is the message composer component. It implements tea.Model and the
// layout.Sizeable/Bindings interfaces.
type Editor struct {
	width  int
	height int

	textarea      textarea.Model
	composerH     int // current composer rows (auto-grows with content)
	lastEsc       time.Time
	deleteMode    bool
	attachments   []Attachment
	onEmptyChange func(bool) // notified when the composer becomes empty/non-empty

	// Slash-command completion. Typing "/" opens an inline menu above the
	// composer listing the available commands, filtered by the prefix typed so
	// far; up/down select, enter runs, esc closes.
	commands   []Command
	completion bool     // the inline command menu is open
	selIndex   int      // selected menu row
}

// Command is one slash command offered by the composer's inline menu. The app
// installs the list via SetCommands; each carries the display text and a short
// description so the user does not have to guess what /foo does. NeedsArg
// commands (e.g. /goal) do not fire on selection — the menu instead fills the
// composer with "/cmd " and waits for the user to type the argument.
type Command struct {
	ID          string
	Title       string
	Description string
	NeedsArg    bool
}

// Attachment is a local stand-in for opencode's message.Attachment. The
// conversation adapter sets Text-only messages today; attachments are kept for
// API compatibility and future file/image input.
type Attachment struct {
	Path     string
	FileName string
}

// SendMsg carries a composed message from the editor to the page.
type SendMsg struct {
	Text        string
	Attachments []Attachment
}

// SessionSelectedMsg notifies the chat components of a session switch. In
// code-agent there is no message service; session switching goes through
// /resume, so this is an informational stub kept for the page wiring.
type SessionSelectedMsg struct {
	ID string
}

// SessionClearedMsg clears the current session transcript state.
type SessionClearedMsg struct{}

func (m *Editor) Init() tea.Cmd {
	return textarea.Blink
}

// openEditor writes the draft to a temp file, opens $EDITOR (default nvim),
// and sends the resulting text on exit.
func (m *Editor) openEditor() tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "nvim"
	}
	tmpfile, err := os.CreateTemp("", "msg_*.md")
	if err != nil {
		return util.ReportError(err)
	}
	name := tmpfile.Name()
	tmpfile.Close()
	c := exec.Command(editor, name) //nolint:gosec
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return util.ReportError(err)
		}
		content, err := os.ReadFile(name)
		if err != nil {
			return util.ReportError(err)
		}
		os.Remove(name)
		if len(content) == 0 {
			return util.ReportWarn("Message is empty")
		}
		attachments := m.attachments
		m.attachments = nil
		return SendMsg{Text: string(content), Attachments: attachments}
	})
}

// send clears the composer and emits SendMsg.
func (m *Editor) send() tea.Cmd {
	value := m.textarea.Value()
	m.textarea.Reset()
	m.syncComposer()
	attachments := m.attachments
	m.attachments = nil
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return tea.Batch(util.CmdHandler(SendMsg{Text: value, Attachments: attachments}))
}

func (m *Editor) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case dialog.ThemeChangedMsg:
		m.textarea = CreateTextArea(&m.textarea)

	case SessionSelectedMsg:
		// Informational: the page owns session state.
		return m, nil

	case SessionClearedMsg:
		m.textarea.Reset()
		m.syncComposer()
		return m, nil

	case tea.KeyMsg:
		if key.Matches(msg, messageKeys.PageUp) || key.Matches(msg, messageKeys.PageDown) ||
			key.Matches(msg, messageKeys.HalfPageUp) || key.Matches(msg, messageKeys.HalfPageDown) {
			// Transcript scroll keys never reach the composer.
			return m, nil
		}

		if key.Matches(msg, editorMaps.OpenEditor) {
			return m, m.openEditor()
		}

		// Slash-command menu is open: up/down move the selection, enter runs the
		// selected command, esc closes the menu. All other keys fall through to
		// the textarea (continuing to type filters the list).
		if m.completion {
			switch {
			case key.Matches(msg, messageKeys.Up):
				m.selIndex = (m.selIndex - 1 + len(m.matchedCommands())) % max(1, len(m.matchedCommands()))
				return m, nil
			case key.Matches(msg, messageKeys.Down):
				m.selIndex = (m.selIndex + 1) % max(1, len(m.matchedCommands()))
				return m, nil
			case key.Matches(msg, editorMaps.Send):
				if len(m.matchedCommands()) > 0 {
					sel := m.matchedCommands()[min(m.selIndex, len(m.matchedCommands())-1)]
					m.completion = false
					m.selIndex = 0
					if sel.NeedsArg {
						// Argument commands: fill the composer with "/cmd " so the
						// user types the argument, then Enter sends it (the textarea
						// keeps the focus for typing). Not executed yet.
						m.textarea.SetValue(sel.Title + " ")
						m.textarea.CursorEnd()
						m.syncComposer()
						return m, nil
					}
					// Fire-and-forget commands execute immediately.
					m.textarea.SetValue(sel.Title)
					m.syncComposer()
					return m, m.send()
				}
				// No match: send the typed text as-is (e.g. a plain message that
				// happens to start with "/").
				m.completion = false
				m.selIndex = 0
				return m, m.send()
			case key.Matches(msg, editorMaps.Escape):
				m.completion = false
				m.selIndex = 0
				return m, nil
			}
		}

		// Shift+Enter inserts a newline at the cursor (the IME-friendly multi-line
		// composer). Plain Enter sends — checked below. Terminal-dependent: only
		// terminals with keyboard enhancement (kitty protocol) report Shift+Enter
		// as a distinct key; without it the modifier is lost and Shift+Enter falls
		// through to the Send path.
		if m.textarea.Focused() && key.Matches(msg, editorMaps.InsertNewline) {
			m.textarea.InsertString("\n")
			m.syncComposer()
			return m, nil
		}

		// Enter sends; a trailing backslash + Enter inserts a newline.
		if m.textarea.Focused() && key.Matches(msg, editorMaps.Send) {
			value := m.textarea.Value()
			if len(value) > 0 && value[len(value)-1] == '\\' {
				m.textarea.SetValue(value[:len(value)-1] + "\n")
				m.syncComposer()
				return m, nil
			}
			return m, m.send()
		}
	}

	m.textarea, cmd = m.textarea.Update(msg)
	// Slash-command completion: typing "/" (or continuing to type after it)
	// opens the filtered command menu; deleting back past "/" closes it.
	m.refreshCompletion()
	// IME sync: keep the composer height matched to its content on every
	// keystroke and on forwarded cursor/blink messages (see syncComposer).
	m.syncComposer()
	m.notifyEmptyChange()
	return m, cmd
}

// refreshCompletion opens or closes the slash-command menu based on the
// composer's current value: "/" plus any prefix filters the available commands;
// anything else (or an empty value) hides it.
func (m *Editor) refreshCompletion() {
	value := m.textarea.Value()
	open := strings.HasPrefix(value, "/")
	if open == m.completion {
		if open {
			m.selIndex = min(m.selIndex, max(0, len(m.matchedCommands())-1))
		}
		return
	}
	m.completion = open
	m.selIndex = 0
}

// matchedCommands returns the commands whose title starts with the typed slash
// prefix. "/" alone matches everything; "/re" narrows to /resume etc.
func (m *Editor) matchedCommands() []Command {
	value := m.textarea.Value()
	prefix := strings.ToLower(strings.TrimPrefix(value, "/"))
	if !strings.HasPrefix(value, "/") || len(m.commands) == 0 {
		return nil
	}
	out := make([]Command, 0, len(m.commands))
	for _, c := range m.commands {
		if strings.HasPrefix(strings.ToLower(c.Title), "/"+prefix) {
			out = append(out, c)
		}
	}
	return out
}

// SetCommands installs the slash commands offered by the inline completion
// menu. The app supplies them at startup; empty disables the menu.
func (m *Editor) SetCommands(cmds []Command) {
	m.commands = cmds
	m.completion = false
	m.selIndex = 0
}

// SetOnEmptyChange registers a callback fired when the composer transitions
// between empty and non-empty, so the chat page can enable/disable transcript
// up/down scrolling.
func (m *Editor) SetOnEmptyChange(fn func(empty bool)) {
	m.onEmptyChange = fn
}

func (m *Editor) notifyEmptyChange() {
	if m.onEmptyChange == nil {
		return
	}
	empty := m.textarea.Value() == ""
	m.onEmptyChange(empty)
}

func (m *Editor) View() tea.View {
	t := theme.CurrentTheme()
	style := lipgloss.NewStyle().
		Padding(0, 0, 0, 1).
		Bold(true).
		Foreground(t.Primary())

	composer := lipgloss.JoinHorizontal(lipgloss.Top, style.Render(">"), m.textarea.View())
	if len(m.attachments) > 0 {
		m.textarea.SetHeight(m.height - 1)
		composer = lipgloss.JoinVertical(lipgloss.Top,
			m.attachmentsContent(),
			lipgloss.JoinHorizontal(lipgloss.Top, style.Render(">"), m.textarea.View()),
		)
	}

	// Inline slash-command menu above the composer.
	menu := m.completionView()
	if menu == "" {
		return tea.NewView(composer)
	}
	return tea.NewView(lipgloss.JoinVertical(lipgloss.Top, menu, composer))
}

// completionView renders the filtered slash-command menu (empty when closed or
// no commands match). The selected row is highlighted.
func (m *Editor) completionView() string {
	if !m.completion {
		return ""
	}
	matches := m.matchedCommands()
	if len(matches) == 0 {
		return ""
	}
	t := theme.CurrentTheme()
	base := styles.BaseStyle()
	var rows []string
	for i, c := range matches {
		title := base.Foreground(t.Text()).Render(c.Title)
		desc := base.Foreground(t.TextMuted()).Render("  " + c.Description)
		if i == m.selIndex {
			title = base.Background(t.Primary()).Foreground(t.Background()).Bold(true).Render(c.Title)
			desc = base.Background(t.Primary()).Foreground(t.Background()).Render("  " + c.Description)
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Left, title, desc))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m *Editor) SetSize(width, height int) tea.Cmd {
	m.width = width
	m.height = height
	m.textarea.SetWidth(composerWidth(width))
	m.textarea.SetHeight(height)
	m.syncComposer()
	return nil
}

func (m *Editor) GetSize() (int, int) {
	return m.textarea.Width(), m.textarea.Height()
}

func (m *Editor) attachmentsContent() string {
	var styledAttachments []string
	t := theme.CurrentTheme()
	attachmentStyles := styles.BaseStyle().
		MarginLeft(1).
		Background(t.TextMuted()).
		Foreground(t.Text())
	for i, attachment := range m.attachments {
		file := attachment.FileName
		if file == "" {
			file = attachment.Path
		}
		var filename string
		if len(file) > 10 {
			filename = fmt.Sprintf(" %s %s...", styles.DocumentIcon, file[0:7])
		} else {
			filename = fmt.Sprintf(" %s %s", styles.DocumentIcon, file)
		}
		if m.deleteMode {
			filename = fmt.Sprintf("%d%s", i, filename)
		}
		styledAttachments = append(styledAttachments, attachmentStyles.Render(filename))
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, styledAttachments...)
}

func (m *Editor) BindingKeys() []key.Binding {
	return layout.KeyMapToSlice(editorMaps)
}

// syncComposer grows/shrinks the composer to fit its content (1..max rows).
// A one-line composer keeps the cursor on the terminal's bottom row, where the
// IME candidate window has room below it — the root fix for the IME overlap.
// Ported from the legacy model.go (same logic, now owned by the editor).
func (m *Editor) syncComposer() {
	promptWidth := runewidth.StringWidth(composerPrompt)
	availableWidth := m.width - composerRightPadding - promptWidth
	if availableWidth < 10 { // defensive: never divide by zero on narrow terminals
		availableWidth = 40
	}
	visualLines := 0
	lines := strings.Split(m.textarea.Value(), "\n")
	for _, line := range lines {
		if line == "" {
			visualLines++
			continue
		}
		w := runewidth.StringWidth(line)
		chunks := (w + availableWidth - 1) / availableWidth
		if chunks == 0 {
			chunks = 1
		}
		visualLines += chunks
	}
	targetHeight := util.Clamp(visualLines, minComposerLines, maxComposerLines)
	if targetHeight != m.composerH {
		m.composerH = targetHeight
		m.textarea.SetHeight(targetHeight)
	}
}

// composerWidth computes the textarea width from the terminal width.
func composerWidth(terminalWidth int) int {
	return util.Clamp(terminalWidth-composerRightPadding, 1, terminalWidth)
}

// CreateTextArea builds a themed textarea, preserving value/size of an
// existing one when non-nil.
func CreateTextArea(existing *textarea.Model) textarea.Model {
	t := theme.CurrentTheme()
	bgColor := t.Background()
	textColor := t.Text()
	textMutedColor := t.TextMuted()
	ta := textarea.New()
	// 使用软件高亮块作为光标，避免 ANSI 物理光标坐标算错抛到最底部
	st := ta.Styles()
	st.Cursor = textarea.CursorStyle{
		Color: lipgloss.Color("205"),
		Shape: tea.CursorBlock,
		Blink: false,
	}
	st.Blurred = textarea.StyleState{
		Base:        styles.BaseStyle().Background(bgColor).Foreground(textColor),
		CursorLine:  styles.BaseStyle().Background(bgColor),
		Placeholder: styles.BaseStyle().Background(bgColor).Foreground(textMutedColor),
		Text:        styles.BaseStyle().Background(bgColor).Foreground(textColor),
	}
	st.Focused = textarea.StyleState{
		Base:        styles.BaseStyle().Background(bgColor).Foreground(textColor),
		CursorLine:  styles.BaseStyle().Background(bgColor),
		Placeholder: styles.BaseStyle().Background(bgColor).Foreground(textMutedColor),
		Text:        styles.BaseStyle().Background(bgColor).Foreground(textColor),
	}
	ta.SetStyles(st)
	ta.Prompt = " "
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	if existing != nil {
		ta.SetValue(existing.Value())
		ta.SetWidth(existing.Width())
		ta.SetHeight(existing.Height())
	}
	ta.Focus()
	return ta
}

// NewEditor creates the composer.
func NewEditor() *Editor {
	ta := CreateTextArea(nil)
	return &Editor{
		textarea:  ta,
		composerH: minComposerLines,
	}
}
