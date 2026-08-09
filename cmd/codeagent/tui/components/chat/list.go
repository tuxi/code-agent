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

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"

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

// SetMessagesMsg replaces the entire transcript with the given messages in
// order (used by /resume, where the full conversation is folded before the
// first render). It subsumes ClearMessagesMsg followed by the new messages:
// one ordered delivery, no races.
type SetMessagesMsg struct {
	Messages []Message
}

// BatchMessagesMsg delivers multiple messages from one event in slice order
// (the conversation adapter folds one event and sends the result as a single
// ordered batch, so the List never sees messages scrambled by concurrent
// command delivery).
type BatchMessagesMsg struct {
	Messages []Message
}

// initialRefreshMsg asks the List to rebuild its viewport just after its first
// transcript content is installed. Terminal.app can miss that first layout
// pass; a subsequent mouse or key event currently happens to repair it.
type initialRefreshMsg struct{}

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

	// Folding. foldState is the user/default open state per fold group; running
	// tool groups force-expand regardless (see isOpen). focusID is the fold row
	// highlighted for keyboard toggle; streamingFoldID is the in-flight thinking
	// block, whose incremental updates ride the stream throttle.
	foldState        map[string]bool
	focusID          string
	streamingFoldID  string
	foldRows         map[string]int // fold ID → first content row (rebuilt by renderView)
	screenX, screenY int            // terminal position of the List's top-left corner

	// IME/drag-free scrolling. Up/Down scroll the viewport only when the
	// composer is empty (the chat page decides this and forwards the keys).
	allowUpDown bool

	// pinned reports whether the viewport should follow streaming output. It
	// starts true; any user scroll away from the bottom clears it, and scrolling
	// back to the bottom re-enables it. Streaming auto-scroll only fires while
	// pinned, so reading history mid-stream is not yanked back to the bottom.
	pinned bool

	// initialRefreshQueued prevents a stream of first-turn events from queuing
	// redundant rebuilds. ClearMessagesMsg and SetMessagesMsg reset it for the
	// next transcript.
	initialRefreshQueued bool

	// Text selection (left-drag). selStart/selCur are content-space cells;
	// selecting is true while the left button is held; dragged distinguishes a
	// drag (copied to the clipboard on release) from a click (toggles the fold
	// under the cursor). lineTable/contentLines mirror the viewport content —
	// the raw rendered lines and their ANSI-stripped text — rebuilt by
	// renderView. copyText is the clipboard writer (tests override it).
	selStart, selCur cellPos
	selecting        bool
	dragged          bool
	lineTable        []string
	contentLines     []string
	copyText         func(string) error
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
	Toggle       key.Binding
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
	Toggle: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "expand/collapse"),
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
		if msg.Message.Kind == KindAssistant && !msg.Message.Finished {
			// Streaming assistant text: defer the full re-render to the
			// spinner-tick throttle (renderThrottle) so per-token updates stay
			// cheap no matter how long the transcript grows. Keep the view
			// pinned to the bottom meanwhile.
			m.scrollToBottomIfPinned()
		} else {
			m.renderView()
			m.scrollToBottomIfPinned()
		}
		return m, m.scheduleInitialRefresh()

	case UpdateMessageMsg:
		m.upsert(msg.Message)
		if (msg.Message.Kind == KindAssistant || (msg.Message.Kind == KindThinking && msg.Message.Fold != nil)) && !msg.Message.Finished {
			// Streaming assistant text or an in-flight thinking block: defer to
			// the stream throttle; keep the view pinned to the bottom only while
			// the latest message is the one streaming.
			if len(m.messages) > 0 {
				last := m.messages[len(m.messages)-1]
				if last.ID == msg.Message.ID {
					m.scrollToBottomIfPinned()
				}
			}
		} else {
			m.renderView()
			// Keep the view pinned to the bottom while the latest message streams.
			if len(m.messages) > 0 {
				last := m.messages[len(m.messages)-1]
				if last.ID == msg.Message.ID {
					m.scrollToBottomIfPinned()
				}
			}
		}
		return m, nil

	case ClearMessagesMsg:
		m.messages = nil
		m.ui = nil
		m.workingID = ""
		m.streamingFoldID = ""
		m.focusID = ""
		m.foldRows = make(map[string]int)
		m.foldState = make(map[string]bool)
		m.cachedContent = make(map[string]cacheItem)
		m.pinned = true
		m.selecting = false
		m.dragged = false
		m.selStart, m.selCur = cellPos{}, cellPos{}
		m.initialRefreshQueued = false
		m.renderView()
		return m, nil

	case SetMessagesMsg:
		// Full replace in one pass: reset every piece of per-message state,
		// then install the new transcript in slice order (upsert on a fresh
		// list appends, so order is preserved by construction).
		m.messages = nil
		m.ui = nil
		m.workingID = ""
		m.streamingFoldID = ""
		m.focusID = ""
		m.foldRows = make(map[string]int)
		m.foldState = make(map[string]bool)
		m.cachedContent = make(map[string]cacheItem)
		m.pinned = true
		m.selecting = false
		m.dragged = false
		m.selStart, m.selCur = cellPos{}, cellPos{}
		m.initialRefreshQueued = false
		for _, msg := range msg.Messages {
			m.upsert(msg)
		}
		m.renderView()
		m.viewport.GotoBottom()
		return m, m.scheduleInitialRefresh()

	case BatchMessagesMsg:
		// Apply one event's messages in slice order. New entries append in
		// order; in-place updates (same ID / Tool.CallID) land on their
		// existing position, so the transcript order is Apply order exactly.
		for _, msg := range msg.Messages {
			m.upsert(msg)
		}
		if len(msg.Messages) > 0 {
			last := msg.Messages[len(msg.Messages)-1]
			if last.Kind == KindAssistant && !last.Finished {
				// Streaming assistant text: defer the full re-render to the
				// spinner-tick throttle; keep the view pinned to the bottom.
				m.scrollToBottomIfPinned()
			} else {
				m.renderView()
				m.scrollToBottomIfPinned()
			}
		}
		return m, m.scheduleInitialRefresh()

	case initialRefreshMsg:
		// Repeat the component-level layout after Bubble Tea has painted the
		// first content frame. This is deliberately local to List: it refreshes
		// both a live first message and a /resume snapshot without changing
		// terminal dimensions or renderer state.
		m.rerender()
		m.scrollToBottomIfPinned()
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
			m.onUserScroll()
		case m.allowUpDown && key.Matches(msg, messageKeys.Toggle):
			// Enter toggles the focused fold row. Guarded by allowUpDown so
			// Enter never fights the composer's send (the split broadcasts keys
			// to both panels; the composer has text when allowUpDown is false).
			if m.focusID != "" {
				m.toggleFold(m.focusID)
			}
		case m.allowUpDown && (key.Matches(msg, messageKeys.Up) || key.Matches(msg, messageKeys.Down)):
			// With fold rows present, up/down move focus between them (Enter
			// toggles). Without folds they fall back to line scrolling.
			if len(m.foldRows) > 0 {
				m.moveFocus(key.Matches(msg, messageKeys.Up))
			} else {
				u, cmd := m.viewport.Update(msg)
				m.viewport = u
				cmds = append(cmds, cmd)
				m.onUserScroll()
			}
		}

	case tea.MouseMsg:
		// Mouse wheel over the transcript scrolls the viewport (the bubbles
		// viewport handles WheelUp/WheelDown natively). A left-drag selects text
		// (reverse-video highlight, copied to the clipboard on release); a left
		// click without drag toggles the fold under the cursor. Program-level
		// mouse capture is enabled in run.go so wheel events reach the app
		// instead of the terminal's scrollback.
		switch ev := msg.(type) {
		case tea.MouseClickMsg:
			// Press: remember the anchor. Whether this becomes a click or a drag
			// is decided on release.
			if ev.Mouse().Button == tea.MouseLeft {
				if p, ok := m.screenToContent(ev.X, ev.Y); ok {
					m.selStart, m.selCur = p, p
					m.selecting = true
					m.dragged = false
				}
			}
		case tea.MouseMotionMsg:
			if m.selecting {
				if p, ok := m.screenToContent(ev.X, ev.Y); ok {
					m.selCur = p
					if p != m.selStart {
						m.dragged = true
					}
					m.applySelection()
				}
				break
			}
			u, cmd := m.viewport.Update(msg)
			m.viewport = u
			cmds = append(cmds, cmd)
			m.onUserScroll()
		case tea.MouseReleaseMsg:
			if m.selecting && ev.Mouse().Button == tea.MouseLeft {
				if m.dragged {
					m.copySelection()
				} else {
					m.handleClick(ev.X, ev.Y)
				}
				m.selecting = false
				m.dragged = false
				m.renderView() // drop the selection highlight
				break
			}
			u, cmd := m.viewport.Update(msg)
			m.viewport = u
			cmds = append(cmds, cmd)
			m.onUserScroll()
		default:
			u, cmd := m.viewport.Update(msg)
			m.viewport = u
			cmds = append(cmds, cmd)
			m.onUserScroll()
		}
	case spinner.TickMsg:
		// Stream throttling: only re-render while a streaming message is live,
		// and at most once per renderThrottle.
		if (m.workingID != "" || m.hasStreamingFold()) && time.Since(lastStreamRender) >= renderThrottle {
			lastStreamRender = time.Now()
			m.renderView()
			if len(m.messages) > 0 && m.messages[len(m.messages)-1].ID == m.workingID {
				m.scrollToBottomIfPinned()
			}
		}
	}

	sp, cmd := m.spinner.Update(msg)
	m.spinner = sp
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m *List) scheduleInitialRefresh() tea.Cmd {
	if m.initialRefreshQueued || len(m.messages) == 0 {
		return nil
	}
	m.initialRefreshQueued = true
	return tea.Tick(20*time.Millisecond, func(time.Time) tea.Msg { return initialRefreshMsg{} })
}

// upsert adds a new message or updates an existing one. Tool cards are matched
// by Tool.CallID (stable identity) so a re-sent card updates in place instead
// of appending a duplicate.
func (m *List) upsert(msg Message) {
	if msg.Fold != nil {
		if _, seen := m.foldState[msg.Fold.ID]; !seen {
			// Default state is collapsed. Running tool groups still display
			// expanded (isOpen forces them) and fold back when they complete.
			m.foldState[msg.Fold.ID] = false
		}
		if msg.Kind == KindThinking && !msg.Finished {
			m.streamingFoldID = msg.ID
		} else if m.streamingFoldID == msg.ID {
			m.streamingFoldID = ""
		}
	}
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

func (m *List) View() tea.View {
	// viewportString() and working() each pad to m.width internally, so both
	// are already full-width rectangles. Joining them is a plain concatenation
	// — a lipgloss.JoinVertical pass would re-measure every line's width over
	// the whole visible window (~300µs per frame) for no effect.
	vp := m.viewportString()
	w := ""
	if len(m.messages) > 0 {
		lastKind := m.messages[len(m.messages)-1].Kind
		if lastKind == KindThinking || lastKind == KindTool {
			w = m.working()
		}
	}

	if w == "" {
		return tea.NewView(vp)
	}
	return tea.NewView(vp + "\n" + w)
}

// viewportString always reads the viewport's current state. A previous memoized
// string could retain the empty startup frame until an unrelated input event
// changed viewport state, leaving the first message or resumed transcript
// visually blank in Terminal.app.
func (m *List) viewportString() string {
	return m.viewport.View()
}

// renderView re-computes the full uiMessage layout from m.messages, reusing
// cached rendered blocks where possible. Visible-range-only rendering is
// handled by the viewport; renderView is cheap for cached blocks.
func (m *List) renderView() {
	m.ui = make([]uiMessage, 0)
	m.foldRows = make(map[string]int)
	pos := 0
	if m.width == 0 {
		return
	}
	for _, msg := range m.messages {
		blocks, ok := m.renderMessage(msg, pos)
		if msg.Fold != nil {
			m.foldRows[msg.Fold.ID] = pos
			// Expanded folds render member blocks; tag them with the fold ID so
			// clicking a member collapses the group again (the summary row is
			// tagged in renderFoldSummary already).
			for i := range blocks {
				blocks[i].foldID = msg.Fold.ID
			}
		}
		for i := range blocks {
			// Cached blocks are re-tagged each pass: position depends only on
			// transcript order, so re-applying it is idempotent and keeps click
			// hit-testing correct even on a cache hit.
			blocks[i].position = pos
		}
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
	m.setContent(
		styles.BaseStyle().
			Width(m.width).
			Render(lipgloss.JoinVertical(lipgloss.Top, messages...)),
	)
}

// scrollToBottomIfPinned pins the viewport to the bottom while new content
// streams in, but only while the user is already at the bottom. Once the user
// scrolls up to read history the follow stops; scrolling back to the bottom
// re-enables it. Every auto-scroll call site goes through this, so streaming
// text, tool cards and the stream throttle all obey the same policy.
func (m *List) scrollToBottomIfPinned() {
	// A drag in progress owns the pointer: don't yank the content while the
	// user is selecting.
	if m.selecting {
		return
	}
	if m.pinned {
		m.viewport.GotoBottom()
	}
}

// onUserScroll re-evaluates the follow-scroll state after the user moves the
// viewport themselves (mouse wheel, page keys, line scroll, fold focus).
// Streaming keeps following only while the viewport stays at the bottom.
func (m *List) onUserScroll() {
	m.pinned = m.viewport.AtBottom()
}

// renderMessage renders one Message into uiMessage blocks, using the
// per-ID cache when the cached width matches. Fold groups render either their
// one-line summary (collapsed) or their members (expanded); the cached block
// list is invalidated on every fold toggle so the two never mix.
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
		if msg.Fold != nil {
			if m.isOpen(msg.Fold) {
				blocks = []uiMessage{renderThinkingMessage(msg, msg.ID == m.streamingFoldID, m.width)}
			} else {
				blocks = []uiMessage{renderFoldSummary(msg.Fold, msg.ID == m.focusID, m.width)}
			}
		} else {
			blocks = []uiMessage{renderThinkingMessage(msg, msg.ID == m.streamingFoldID, m.width)}
		}
	case KindTool:
		if msg.Fold != nil {
			if m.isOpen(msg.Fold) {
				for i := range msg.Fold.ToolCalls {
					blocks = append(blocks, renderToolMessage(msg.Fold.ToolCalls[i], m.width))
				}
			} else {
				blocks = []uiMessage{renderFoldSummary(msg.Fold, msg.ID == m.focusID, m.width)}
			}
		} else if msg.Tool != nil {
			blocks = []uiMessage{renderToolMessage(*msg.Tool, m.width)}
		}
	case KindSystem:
		if msg.Fold != nil {
			if m.isOpen(msg.Fold) {
				for _, ln := range msg.Fold.Lines {
					blocks = append(blocks, renderSystemMessage(Message{Kind: KindSystem, Content: ln}, m.width))
				}
			} else {
				blocks = []uiMessage{renderFoldSummary(msg.Fold, msg.ID == m.focusID, m.width)}
			}
		} else {
			blocks = []uiMessage{renderSystemMessage(msg, m.width)}
		}
	}
	m.cachedContent[msg.ID] = cacheItem{width: m.width, content: blocks}
	return blocks, true
}

// rerender drops every cached block (theme change) and rebuilds the layout.
func (m *List) rerender() {
	m.cachedContent = make(map[string]cacheItem)
	m.renderView()
}

// --- fold interaction -------------------------------------------------------

// isOpen reports whether a fold group is currently expanded. Running tool
// groups stay expanded so work in flight is always visible; everything else
// follows the user's last toggle (default collapsed).
func (m *List) isOpen(f *Fold) bool {
	if len(f.ToolCalls) > 0 && f.Running {
		return true
	}
	return m.foldState[f.ID]
}

// toggleFold flips a fold group's open state. Running tool groups are pinned
// open (they fold back when the last member finishes) and ignore the toggle.
func (m *List) toggleFold(id string) {
	for _, msg := range m.messages {
		if msg.Fold != nil && msg.Fold.ID == id {
			if len(msg.Fold.ToolCalls) > 0 && msg.Fold.Running {
				return
			}
			m.foldState[id] = !m.isOpen(msg.Fold)
			delete(m.cachedContent, id)
			m.renderView()
			return
		}
	}
}

// moveFocus shifts the keyboard highlight to the previous/next fold row,
// wrapping around. The focused row is scrolled into view and its summary
// re-renders highlighted.
func (m *List) moveFocus(up bool) {
	ids := make([]string, 0, 8)
	for _, msg := range m.messages {
		if msg.Fold != nil {
			ids = append(ids, msg.Fold.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	idx := -1
	for i, id := range ids {
		if id == m.focusID {
			idx = i
			break
		}
	}
	switch {
	case idx == -1:
		if up {
			idx = len(ids) - 1
		} else {
			idx = 0
		}
	case up:
		idx--
		if idx < 0 {
			idx = len(ids) - 1
		}
	default:
		idx++
		if idx >= len(ids) {
			idx = 0
		}
	}
	old := m.focusID
	m.focusID = ids[idx]
	delete(m.cachedContent, old)
	delete(m.cachedContent, m.focusID)
	m.renderView()
	m.scrollToFocus()
	m.onUserScroll()
}

// scrollToFocus keeps the focused fold row inside the viewport.
func (m *List) scrollToFocus() {
	if y, ok := m.foldRows[m.focusID]; ok {
		m.viewport.EnsureVisible(y, 0, 0)
	}
}

// handleClick maps a terminal click onto the transcript and toggles the fold
// whose summary or expanded member block it landed on, if any. screenX/screenY
// anchor the List's top-left corner (set by the chat page from the layout).
func (m *List) handleClick(x, y int) {
	lx := x - m.screenX
	ly := y - m.screenY
	if lx < 0 || lx >= m.width || ly < 0 || ly >= m.height {
		return
	}
	row := ly + m.viewport.YOffset()
	for _, u := range m.ui {
		// Both the collapsed summary row and the expanded member blocks belong
		// to a fold; clicking either toggles it (running tool groups refuse).
		if u.foldID == "" || u.messageType == foldSummaryMessageType && u.ID == "" {
			continue
		}
		if row >= u.position && row < u.position+u.height {
			m.toggleFold(u.foldID)
			return
		}
	}
}

// hasStreamingFold reports whether an in-flight thinking block still exists,
// clearing the stale ID when the block finished without a final update.
func (m *List) hasStreamingFold() bool {
	if m.streamingFoldID == "" {
		return false
	}
	for _, msg := range m.messages {
		if msg.ID == m.streamingFoldID && msg.Kind == KindThinking && !msg.Finished {
			return true
		}
	}
	m.streamingFoldID = ""
	return false
}

// SetScreenOrigin records the terminal position of the List's top-left corner
// so mouse clicks can be mapped to transcript rows. The chat page computes it
// from the layout composition (padding/borders above and left of the list).
func (m *List) SetScreenOrigin(x, y int) {
	m.screenX, m.screenY = x, y
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
	m.viewport.SetWidth(width)
	m.viewport.SetHeight(max(0, height))
	// Width changed: all cached blocks are invalid.
	if m.viewport.Width() != 0 {
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
		messageKeys.Toggle,
	}
}

// NewList creates an empty message transcript.
func NewList() *List {
	s := spinner.New()
	s.Spinner = spinner.Pulse
	vp := viewport.New()
	vp.KeyMap.PageUp = messageKeys.PageUp
	vp.KeyMap.PageDown = messageKeys.PageDown
	vp.KeyMap.HalfPageUp = messageKeys.HalfPageUp
	vp.KeyMap.HalfPageDown = messageKeys.HalfPageDown
	return &List{
		cachedContent: make(map[string]cacheItem),
		foldState:     make(map[string]bool),
		foldRows:      make(map[string]int),
		viewport:      vp,
		spinner:       s,
		pinned:        true,
		copyText:      clipboard.WriteAll,
	}
}
