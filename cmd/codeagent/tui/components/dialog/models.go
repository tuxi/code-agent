package dialog

import (
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"code-agent/cmd/codeagent/tui/layout"
	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/cmd/codeagent/tui/util"
)

const (
	numVisibleModels = 10
	maxDialogWidth   = 40
)

// ModelOption is one selectable model. The app builds these from its own
// model list and maps a selection back through the existing modelSwap path.
type ModelOption struct {
	Name    string // the model key (what modelSwap expects)
	Display string // human-readable label shown in the picker
}

// ModelSelectedMsg is sent when a model is selected.
type ModelSelectedMsg struct {
	Name string
}

// CloseModelDialogMsg is sent when the model dialog is closed.
type CloseModelDialogMsg struct{}

// ModelDialog interface for the model selection dialog
type ModelDialog interface {
	tea.Model
	layout.Bindings
}

type modelDialogCmp struct {
	models        []ModelOption
	selectedIdx   int
	width         int
	height        int
	scrollOffset  int
}

type modelKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Enter  key.Binding
	Escape key.Binding
	J      key.Binding
	K      key.Binding
}

var modelKeys = modelKeyMap{
	Up: key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("↑", "previous model"),
	),
	Down: key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "next model"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "select model"),
	),
	Escape: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "close"),
	),
	J: key.NewBinding(
		key.WithKeys("j"),
		key.WithHelp("j", "next model"),
	),
	K: key.NewBinding(
		key.WithKeys("k"),
		key.WithHelp("k", "previous model"),
	),
}

func (m *modelDialogCmp) Init() tea.Cmd {
	return nil
}

func (m *modelDialogCmp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, modelKeys.Up) || key.Matches(msg, modelKeys.K):
			m.moveSelectionUp()
		case key.Matches(msg, modelKeys.Down) || key.Matches(msg, modelKeys.J):
			m.moveSelectionDown()
		case key.Matches(msg, modelKeys.Enter):
			if len(m.models) == 0 {
				return m, nil
			}
			name := m.models[m.selectedIdx].Name
			util.ReportInfo(fmt.Sprintf("selected model: %s", name))
			return m, util.CmdHandler(ModelSelectedMsg{Name: name})
		case key.Matches(msg, modelKeys.Escape):
			return m, util.CmdHandler(CloseModelDialogMsg{})
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}

	return m, nil
}

// moveSelectionUp moves the selection up or wraps to bottom
func (m *modelDialogCmp) moveSelectionUp() {
	if len(m.models) == 0 {
		return
	}
	if m.selectedIdx > 0 {
		m.selectedIdx--
	} else {
		m.selectedIdx = len(m.models) - 1
		m.scrollOffset = max(0, len(m.models)-numVisibleModels)
	}

	// Keep selection visible
	if m.selectedIdx < m.scrollOffset {
		m.scrollOffset = m.selectedIdx
	}
}

// moveSelectionDown moves the selection down or wraps to top
func (m *modelDialogCmp) moveSelectionDown() {
	if len(m.models) == 0 {
		return
	}
	if m.selectedIdx < len(m.models)-1 {
		m.selectedIdx++
	} else {
		m.selectedIdx = 0
		m.scrollOffset = 0
	}

	// Keep selection visible
	if m.selectedIdx >= m.scrollOffset+numVisibleModels {
		m.scrollOffset = m.selectedIdx - (numVisibleModels - 1)
	}
}

func (m *modelDialogCmp) View() string {
	t := theme.CurrentTheme()
	baseStyle := styles.BaseStyle()

	title := baseStyle.
		Foreground(t.Primary()).
		Bold(true).
		Width(maxDialogWidth).
		Padding(0, 0, 1).
		Render("Select Model")

	// Render visible models
	endIdx := min(m.scrollOffset+numVisibleModels, len(m.models))
	modelItems := make([]string, 0, endIdx-m.scrollOffset)

	for i := m.scrollOffset; i < endIdx; i++ {
		itemStyle := baseStyle.Width(maxDialogWidth)
		if i == m.selectedIdx {
			itemStyle = itemStyle.Background(t.Primary()).
				Foreground(t.Background()).Bold(true)
		}
		display := m.models[i].Display
		if display == "" {
			display = m.models[i].Name
		}
		modelItems = append(modelItems, itemStyle.Render(display))
	}

	scrollIndicator := m.getScrollIndicators(maxDialogWidth)

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		baseStyle.Width(maxDialogWidth).Render(lipgloss.JoinVertical(lipgloss.Left, modelItems...)),
		scrollIndicator,
	)

	return baseStyle.Padding(1, 2).
		Border(lipgloss.RoundedBorder()).
		BorderBackground(t.Background()).
		BorderForeground(t.TextMuted()).
		Width(lipgloss.Width(content) + 4).
		Render(content)
}

func (m *modelDialogCmp) getScrollIndicators(maxWidth int) string {
	var indicator string

	if len(m.models) > numVisibleModels {
		if m.scrollOffset > 0 {
			indicator += "↑ "
		}
		if m.scrollOffset+numVisibleModels < len(m.models) {
			indicator += "↓ "
		}
	}

	if indicator == "" {
		return ""
	}

	t := theme.CurrentTheme()
	baseStyle := styles.BaseStyle()

	return baseStyle.
		Foreground(t.Primary()).
		Width(maxWidth).
		Align(lipgloss.Right).
		Bold(true).
		Render(indicator)
}

func (m *modelDialogCmp) BindingKeys() []key.Binding {
	return layout.KeyMapToSlice(modelKeys)
}

// NewModelDialogCmp creates the model selection dialog over a flat list of
// options supplied by the app.
func NewModelDialogCmp(models []ModelOption) ModelDialog {
	return &modelDialogCmp{
		models:       models,
		selectedIdx:  0,
		scrollOffset: 0,
	}
}

