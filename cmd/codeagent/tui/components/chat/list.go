// Package chat implements the chat transcript and composer components of the
// TUI, ported from opencode (internal/tui/components/chat) and adapted to
// code-agent's local infrastructure.
//
// List is the scrollable message transcript. It renders Message values (see
// message.go) into a viewport, caches per-message rendered content keyed by
// message ID, and only re-renders the visible range when scrolling. Streaming
// (Finished=false) assistant messages are re-rendered on a throttle; finalized
// messages never re-enter the streaming path.
package chat

import (
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"code-agent/cmd/codeagent/tui/components/dialog"
	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/theme"
)

// NewMessageMsg is sent by the conversation adapter when a message is created
// (appended to the transcript).
type NewMessageMsg struct {
	Message Message
}

// UpdateMessageMsg is sent by the conversation adapter when an existing
// message changes (streaming assistant text, tool card status/result updates).
// Tool cards are matched by Tool.CallID so they update in place.
type UpdateMessageMsg struct {
	Message Message
}

// ClearMessagesMsg clears the transcript (e.g. a /new session).
type ClearMessagesMsg struct{}

// List is the message transcript component. It implements tea.Model and the
// layout.Sizeable/Bindings interfaces.
type List struct {
	width, height int

	viewport viewport.Model
	messages []Message
	ui       []uiMessage

	// cachedContent caches rendered uiMessage blocks per message ID and width.
	cachedContent map[string]cacheItem

	spinner   spinner.Model
	workingID string // ID of the currently streaming assistant message, if any

	// IME/drag-free scrolling. Up/Down scroll the viewport only when the
	// composer is empty (the chat page decides this and forwards the keys).
	allowUpDown bool
}

// cacheItem is a cached rendered message block, valid only at one width.
type cacheItem struct {
	width   int
	content []uiMessage
}

// MessageKeys are the transcript scroll bindings.
type MessageKeys struct {
	PageDown     key.Binding
	PageUp       key.Binding
	HalfPageUp   key.Binding
	HalfPageDown key.Binding
	Up           key.Binding
	Down         key.Binding
}

var messageKeys = MessageKeys{
	PageDown: key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("f/pgdn", "page down"),
	),
	PageUp: key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("b/pgup", "page up"),
	),
	HalfPageUp: key.NewBinding(
		key.WithKeys("ctrl+u"),
		key.WithHelp("ctrl+u", "½ page up"),
	),
	HalfPageDown: key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("ctrl+d", "½ page down"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "line down"),
	),
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "line up"),
	),
}

func (m *List) Init() tea.Cmd {
	return tea.Batch(m.viewport.Init(), m.spinner.Tick)
}

// renderThrottle is the minimum interval between streaming re-renders (see
// plan §8.2 streaming throttling). Streaming re-render is driven off spinner
// ticks gated by this interval.
const renderThrottle = 80 * time.Millisecond

// lastStreamRender is the timestamp of the last streaming re-render.
var lastStreamRender time.Time

func (m *List) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case dialog.ThemeChangedMsg:
		m.rerender()
		return m, nil

	case NewMessageMsg:
		m.upsert(msg.Message)
		m.renderView()
		m.viewport.GotoBottom()
		return m, nil

	case UpdateMessageMsg:
		m.upsert(msg.Message)
		m.renderView()
		// Keep the view pinned to the bottom while the latest message streams.
		if len(m.messages) > 0 {
			last := m.messages[len(m.messages)-1]
			if last.ID == msg.Message.ID {
				m.viewport.GotoBottom()
			}
		}
		return m, nil

	case ClearMessagesMsg:
		m.messages = nil
		m.ui = nil
		m.workingID = ""
		m.cachedContent = make(map[string]cacheItem)
		m.renderView()
		return m, nil

	case tea.KeyMsg:
		// Scroll keys are always handled here; up/down only when the composer
		// is empty (the chat page toggles allowUpDown).
		switch {
		case key.Matches(msg, messageKeys.PageUp),
			key.Matches(msg, messageKeys.PageDown),
			key.Matches(msg, messageKeys.HalfPageUp),
			key.Matches(msg, messageKeys.HalfPageDown):
			u, cmd := m.viewport.Update(msg)
			m.viewport = u
			cmds = append(cmds, cmd)
		case m.allowUpDown && (key.Matches(msg, messageKeys.Up) || key.Matches(msg, messageKeys.Down)):
			u, cmd := m.viewport.Update(msg)
			m.viewport = u
			cmds = append(cmds, cmd)
		}

	case tea.MouseMsg:
		// Mouse wheel over the transcript scrolls the viewport (the bubbles
		// viewport handles WheelUp/WheelDown natively; clicks fall through).
		// Program-level mouse capture is enabled in run.go so wheel events
		// reach the app instead of the terminal's scrollback.
		// 让 Viewport 内部自行处理 LineUp/LineDown，不要重新触发全量 ViewModel 重构
		u, cmd := m.viewport.Update(msg)
		m.viewport = u
		cmds = append(cmds, cmd)
	case spinner.TickMsg:
		// Stream throttling: only re-render while a streaming message is live,
		// and at most once per renderThrottle.
		if m.workingID != "" && time.Since(lastStreamRender) >= renderThrottle {
			lastStreamRender = time.Now()
			m.renderView()
			if len(m.messages) > 0 && m.messages[len(m.messages)-1].ID == m.workingID {
				m.viewport.GotoBottom()
			}
		}
	}

	sp, cmd := m.spinner.Update(msg)
	m.spinner = sp
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// upsert adds a new message or updates an existing one. Tool cards are matched
// by Tool.CallID (stable identity) so a re-sent card updates in place instead
// of appending a duplicate.
func (m *List) upsert(msg Message) {
	if msg.Kind == KindTool && msg.Tool != nil {
		for i, existing := range m.messages {
			if existing.Kind == KindTool && existing.Tool != nil &&
				existing.Tool.CallID == msg.Tool.CallID {
				m.messages[i] = msg
				delete(m.cachedContent, existing.ID)
				delete(m.cachedContent, msg.ID)
				return
			}
		}
	}
	for i, existing := range m.messages {
		if existing.ID == msg.ID {
			m.messages[i] = msg
			delete(m.cachedContent, msg.ID)
			return
		}
	}
	m.messages = append(m.messages, msg)
	if msg.Kind == KindAssistant && !msg.Finished {
		m.workingID = msg.ID
	} else if m.workingID == msg.ID {
		m.workingID = ""
	}
}

func (m *List) View() string {
	baseStyle := styles.BaseStyle()

	content := baseStyle.
		Width(m.width).
		Render(lipgloss.JoinVertical(lipgloss.Top, m.viewport.View(), m.working()))

	return baseStyle.
		Width(m.width).
		Render(content)
}

// renderView re-computes the full uiMessage layout from m.messages, reusing
// cached rendered blocks where possible. Visible-range-only rendering is
// handled by the viewport; renderView is cheap for cached blocks.
func (m *List) renderView() {
	m.ui = make([]uiMessage, 0)
	pos := 0
	if m.width == 0 {
		return
	}
	for _, msg := range m.messages {
		blocks, ok := m.renderMessage(msg, pos)
		m.ui = append(m.ui, blocks...)
		for _, b := range blocks {
			pos += b.height + 1 // + 1 for spacing
		}
		_ = ok
	}
	messages := make([]string, 0, len(m.ui))
	for _, v := range m.ui {
		messages = append(messages, lipgloss.JoinVertical(lipgloss.Left, v.content))
		messages = append(messages, styles.BaseStyle().Width(m.width).Render(""))
	}
	m.viewport.SetContent(
		styles.BaseStyle().
			Width(m.width).
			Render(lipgloss.JoinVertical(lipgloss.Top, messages...)),
	)
}

// renderMessage renders one Message into uiMessage blocks, using the
// per-ID cache when the cached width matches.
func (m *List) renderMessage(msg Message, pos int) ([]uiMessage, bool) {
	if cache, ok := m.cachedContent[msg.ID]; ok && cache.width == m.width {
		return cache.content, true
	}
	var blocks []uiMessage
	switch msg.Kind {
	case KindUser:
		blocks = []uiMessage{renderUserMessage(msg, false, m.width, pos)}
	case KindAssistant:
		blocks = renderAssistantMessage(msg, m.width, pos)
	case KindCompact:
		blocks = []uiMessage{renderCompactMessage(msg, m.width)}
	case KindThinking:
		blocks = []uiMessage{renderThinkingMessage(msg, msg.ID == m.workingID, m.width)}
	case KindTool:
		if msg.Tool == nil {
			return nil, false
		}
		blocks = []uiMessage{renderToolMessage(*msg.Tool, m.width)}
	case KindSystem:
		blocks = []uiMessage{renderSystemMessage(msg, m.width)}
	}
	m.cachedContent[msg.ID] = cacheItem{width: m.width, content: blocks}
	return blocks, true
}

// rerender drops every cached block (theme change) and rebuilds the layout.
func (m *List) rerender() {
	m.cachedContent = make(map[string]cacheItem)
	m.renderView()
}

func (m *List) working() string {
	if m.workingID == "" {
		return ""
	}
	t := theme.CurrentTheme()
	text := styles.BaseStyle().
		Width(m.width).
		Foreground(t.Primary()).
		Bold(true).
		Render(m.spinner.View() + " Generating...")
	return text
}

// SetSize implements layout.Sizeable.
func (m *List) SetSize(width, height int) tea.Cmd {
	if m.width == width && m.height == height {
		return nil
	}
	m.width = width
	m.height = height
	m.viewport.Width = width
	m.viewport.Height = max(0, height)
	// Width changed: all cached blocks are invalid.
	if m.viewport.Width != 0 {
		m.rerender()
	}
	return nil
}

func (m *List) GetSize() (int, int) {
	return m.width, m.height
}

// SetAllowUpDown enables line up/down scrolling. The chat page enables it only
// while the composer is empty so arrow keys edit text otherwise.
func (m *List) SetAllowUpDown(allow bool) {
	m.allowUpDown = allow
}

// BindingKeys implements layout.Bindings.
func (m *List) BindingKeys() []key.Binding {
	return []key.Binding{
		m.viewport.KeyMap.PageDown,
		m.viewport.KeyMap.PageUp,
		m.viewport.KeyMap.HalfPageUp,
		m.viewport.KeyMap.HalfPageDown,
		messageKeys.Up,
		messageKeys.Down,
	}
}

// NewList creates an empty message transcript.
func NewList() *List {
	s := spinner.New()
	s.Spinner = spinner.Pulse
	vp := viewport.New(0, 0)
	vp.KeyMap.PageUp = messageKeys.PageUp
	vp.KeyMap.PageDown = messageKeys.PageDown
	vp.KeyMap.HalfPageUp = messageKeys.HalfPageUp
	vp.KeyMap.HalfPageDown = messageKeys.HalfPageDown
	return &List{
		cachedContent: make(map[string]cacheItem),
		viewport:      vp,
		spinner:       s,
	}
}
