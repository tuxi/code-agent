package core

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/cmd/codeagent/tui/util"
)

// StatusCmp is the bottom status bar. It is driven entirely by the parent app
// through setters — no LSP clients, no opencode session/config/pubsub plumbing.
// The parent owns those data sources and pushes plain values in; the component
// only renders.
type StatusCmp interface {
	tea.Model

	SetModel(model string)
	SetTokens(tokens int64)
	SetGit(git string)
	SetBusy(busy bool)
}

type statusCmp struct {
	info       util.InfoMsg
	width      int
	messageTTL time.Duration

	model  string
	tokens int64
	git    string
	busy   bool
}

// clearMessageCmd is a command that clears status messages after a timeout
func (m *statusCmp) clearMessageCmd(ttl time.Duration) tea.Cmd {
	return tea.Tick(ttl, func(time.Time) tea.Msg {
		return util.ClearStatusMsg{}
	})
}

func (m *statusCmp) Init() tea.Cmd {
	return nil
}

// Update uses a pointer receiver so the component's mutated state (width,
// info) lives on the same *statusCmp the app drives through its setters. A
// value receiver would copy the struct through the layout's Model interface on
// the first Update, after which SetModel/SetTokens/SetBusy would mutate a
// different object than the one being rendered — freezing the status bar.
func (m *statusCmp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case util.InfoMsg:
		m.info = msg
		ttl := msg.TTL
		if ttl == 0 {
			ttl = m.messageTTL
		}
		return m, m.clearMessageCmd(ttl)
	case util.ClearStatusMsg:
		m.info = util.InfoMsg{}
	}
	return m, nil
}

func (m *statusCmp) SetModel(model string)  { m.model = model }
func (m *statusCmp) SetTokens(tokens int64) { m.tokens = tokens }
func (m *statusCmp) SetGit(git string)      { m.git = git }
func (m *statusCmp) SetBusy(busy bool)      { m.busy = busy }

var helpWidget = ""

// getHelpWidget returns the help widget with current theme colors
func getHelpWidget() string {
	t := theme.CurrentTheme()
	helpText := "ctrl+? help"

	return styles.Padded().
		Background(t.TextMuted()).
		Foreground(t.BackgroundDarker()).
		Bold(true).
		Render(helpText)
}

// formatTokens renders a token count in human-readable form (e.g., 110K, 1.2M).
func formatTokens(tokens int64) string {
	switch {
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%.1fK", float64(tokens)/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

func (m *statusCmp) View() tea.View {
	t := theme.CurrentTheme()

	// Initialize the help widget
	status := getHelpWidget()

	gitWidth := 0
	// 暂时不显示git，显得有点乱
	//if m.git != "" {
	//	git := styles.Padded().
	//		Background(t.BackgroundDarker()).
	//		Foreground(t.Text()).
	//		Render(m.git)
	//	gitWidth = lipgloss.Width(git) + 2
	//	status += git
	//}

	tokenInfoWidth := 0
	if m.tokens > 0 {
		tokens := styles.Padded().
			Background(t.Text()).
			Foreground(t.BackgroundSecondary()).
			Render(formatTokens(m.tokens))
		tokenInfoWidth = lipgloss.Width(tokens) + 2
		status += tokens
	}

	modelWidth := 0
	if m.model != "" {
		model := styles.Padded().
			Background(t.Secondary()).
			Foreground(t.Background()).
			Render(m.model)
		modelWidth = lipgloss.Width(model) + 2
		status += model
	}

	availableWidht := max(0, m.width-lipgloss.Width(helpWidget)-gitWidth-tokenInfoWidth-modelWidth)

	if m.info.Msg != "" {
		infoStyle := styles.Padded().
			Foreground(t.Background()).
			Width(availableWidht)

		switch m.info.Type {
		case util.InfoTypeInfo:
			infoStyle = infoStyle.Background(t.Info())
		case util.InfoTypeWarn:
			infoStyle = infoStyle.Background(t.Warning())
		case util.InfoTypeError:
			infoStyle = infoStyle.Background(t.Error())
		}

		infoWidth := availableWidht - 10
		// Truncate message if it's longer than available width
		msg := m.info.Msg
		if len(msg) > infoWidth && infoWidth > 0 {
			msg = msg[:infoWidth] + "..."
		}
		status += infoStyle.Render(msg)
	} else if m.busy {
		status += styles.Padded().
			Foreground(t.Background()).
			Background(t.Info()).
			Width(availableWidht).
			Render("working…")
	} else {
		status += styles.Padded().
			Foreground(t.Text()).
			Background(t.BackgroundSecondary()).
			Width(availableWidht).
			Render("")
	}

	return tea.NewView(status)
}

func NewStatusCmp() StatusCmp {
	helpWidget = getHelpWidget()

	return &statusCmp{
		messageTTL: 10 * time.Second,
	}
}
