// Package page defines the top-level TUI pages and the message used to switch
// between them. Ported from opencode (internal/tui/page) and adapted to
// code-agent's local infrastructure.
//
// ChatPage is the main chat page: a SplitPane with the message transcript on
// top and the composer at the bottom. opencode's session sidebar, completions
// dialog and agent-run wiring are dropped — code-agent has no session/message
// service; the conversation adapter (conversation.go) fills the transcript
// with Message values and the page forwards SendMsg to it.
package page

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"code-agent/cmd/codeagent/tui/components/chat"
	"code-agent/cmd/codeagent/tui/layout"
	"code-agent/cmd/codeagent/tui/util"
)

// ChatPage is the PageID of the chat page.
var ChatPage PageID = "chat"

// chatPage is the main conversation page.
type chatPage struct {
	editor   layout.Container
	messages layout.Container
	layout   layout.SplitPaneLayout

	// list is the transcript component inside messages; kept separately so the
	// page can feed it its screen origin (click hit-testing) on resize.
	list *chat.List

	// onSubmit is called with the composed text. The conversation adapter
	// installs it; when nil the text is silently discarded.
	onSubmit func(text string) tea.Cmd
}

// ChatKeyMap are the page-level bindings.
type ChatKeyMap struct {
	NewSession key.Binding
	Cancel     key.Binding
}

var keyMap = ChatKeyMap{
	NewSession: key.NewBinding(
		key.WithKeys("ctrl+n"),
		key.WithHelp("ctrl+n", "new session"),
	),
	Cancel: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "cancel"),
	),
}

func (p *chatPage) Init() tea.Cmd {
	return p.layout.Init()
}

func (p *chatPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := p.layout.SetSize(msg.Width, msg.Height)
		p.syncScreenOrigin()
		cmds = append(cmds, cmd)
		return p, tea.Batch(cmds...)

	case chat.SendMsg:
		if p.onSubmit != nil {
			if cmd := p.onSubmit(msg.Text); cmd != nil {
				return p, cmd
			}
		}
		// Forward to the layout (editor) so it can update its internal state
		// even when the submit handler is absent.
		u, cmd := p.layout.Update(msg)
		p.layout = u.(layout.SplitPaneLayout)
		return p, tea.Batch(append(cmds, cmd)...)

	case chat.SessionClearedMsg:
		u, cmd := p.layout.Update(msg)
		p.layout = u.(layout.SplitPaneLayout)
		return p, tea.Batch(append(cmds, cmd)...)

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keyMap.NewSession):
			return p, tea.Batch(util.CmdHandler(chat.SessionClearedMsg{}))
		case key.Matches(msg, keyMap.Cancel):
			// No agent-run wiring in code-agent: cancel is informational.
			return p, nil
		}
	}

	u, cmd := p.layout.Update(msg)
	cmds = append(cmds, cmd)
	p.layout = u.(layout.SplitPaneLayout)
	return p, tea.Batch(cmds...)
}

func (p *chatPage) View() tea.View {
	// Re-sync before every render. The split pane above consumes
	// tea.WindowSizeMsg without forwarding it to the page, so relying on the
	// WindowSizeMsg case alone would leave the transcript's screen origin at
	// (0,0) and mis-map every click by the container's top padding.
	p.syncScreenOrigin()
	return p.layout.View()
}

func (p *chatPage) SetSize(width, height int) tea.Cmd {
	return p.layout.SetSize(width, height)
}

func (p *chatPage) GetSize() (int, int) {
	return p.layout.GetSize()
}

func (p *chatPage) BindingKeys() []key.Binding {
	bindings := layout.KeyMapToSlice(keyMap)
	bindings = append(bindings, p.messages.BindingKeys()...)
	bindings = append(bindings, p.editor.BindingKeys()...)
	return bindings
}

// SetOnSubmit installs the conversation adapter's submit handler.
func (p *chatPage) SetOnSubmit(fn func(text string) tea.Cmd) {
	p.onSubmit = fn
}

// SetHeader installs the brand line rendered above the first transcript
// message (scrolls with the conversation, never pinned).
func (p *chatPage) SetHeader(h string) {
	p.list.SetHeader(h)
}

// syncScreenOrigin tells the transcript where its top-left corner sits on the
// terminal, so mouse clicks map to rows. The page is the app's top-left panel
// (origin 0,0) and the messages container adds its top/left padding (1,1) —
// the editor and status bar sit below the list, never above or beside it.
func (p *chatPage) syncScreenOrigin() {
	p.list.SetScreenOrigin(1, 1)
}

// NewChatPage creates the chat page: transcript on top, composer at the bottom.
func NewChatPage() *chatPage {
	msgList := chat.NewList()
	editor := chat.NewEditor()

	// Up/down scroll the transcript only while the composer is empty, so arrow
	// keys edit text otherwise. The editor reports its emptiness transitions.
	editor.SetOnEmptyChange(func(empty bool) {
		msgList.SetAllowUpDown(empty)
	})

	messagesContainer := layout.NewContainer(
		msgList,
		layout.WithPadding(1, 1, 0, 1),
	)
	editorContainer := layout.NewContainer(
		editor,
		layout.WithBorder(true, false, false, false),
	)

	return &chatPage{
		editor:   editorContainer,
		messages: messagesContainer,
		list:     msgList,
		layout: layout.NewSplitPane(
			layout.WithLeftPanel(messagesContainer),
			layout.WithBottomPanel(editorContainer),
		),
	}
}
