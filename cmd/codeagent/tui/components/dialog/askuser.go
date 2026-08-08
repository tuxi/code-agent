package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"code-agent/cmd/codeagent/tui/layout"
	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/cmd/codeagent/tui/util"
	"code-agent/internal/agent"
)

// CloseAskUserMsg tells the app the ask_user dialog answered so the app can
// close it. The answer itself has already been delivered on the request's
// Reply channel before this message is emitted.
type CloseAskUserMsg struct{}

// AskUserRequest is the dialog's view of a pending clarification question: the
// question and the channel the runner goroutine blocks on until the user
// answers (or cancels).
type AskUserRequest struct {
	Question agent.AskUserQuestion
	Reply    chan agent.AskUserAnswer
}

// AskUserDialogCmp is the clarification dialog.
type AskUserDialogCmp interface {
	tea.Model
	layout.Bindings
	SetQuestion(req AskUserRequest) tea.Cmd
}

type askUserKeyMap struct {
	Up      key.Binding
	Down    key.Binding
	Toggle  key.Binding
	Confirm key.Binding
	Cancel  key.Binding
}

var askUserKeys = askUserKeyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k", "ctrl+p"),
		key.WithHelp("↑/k", "navigate"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j", "ctrl+n"),
		key.WithHelp("↓/j", "navigate"),
	),
	Toggle: key.NewBinding(
		key.WithKeys(" "),
		key.WithHelp("space", "toggle"),
	),
	Confirm: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "confirm"),
	),
	Cancel: key.NewBinding(
		key.WithKeys("esc", "escape", "ctrl+c"),
		key.WithHelp("esc", "cancel"),
	),
}

// askUserDialogCmp is the clarification dialog. When AllowCustom is on, index 0
// is the custom-text input and options are 1..N; otherwise options are 0..N-1.
type askUserDialogCmp struct {
	width           int
	height          int
	req             AskUserRequest
	windowSize      tea.WindowSizeMsg
	contentViewport viewport.Model
	selected        int          // highlighted option index (↑/↓)
	multi           map[int]bool // selected indices when multiSelect is on
}

func (a *askUserDialogCmp) Init() tea.Cmd {
	return a.contentViewport.Init()
}

// optionCount is len(Options) plus the custom-text row when AllowCustom is on.
func (a *askUserDialogCmp) optionCount() int {
	n := len(a.req.Question.Options)
	if a.req.Question.AllowCustom {
		return n + 1
	}
	return n
}

func (a *askUserDialogCmp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.windowSize = msg
		return a, a.SetSize()
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, askUserKeys.Up):
			if a.selected > 0 {
				a.selected--
			}
			return a, nil
		case key.Matches(msg, askUserKeys.Down):
			if a.selected < a.optionCount()-1 {
				a.selected++
			}
			return a, nil
		case key.Matches(msg, askUserKeys.Toggle):
			if a.req.Question.MultiSelect {
				if a.multi == nil {
					a.multi = make(map[int]bool)
				}
				a.multi[a.selected] = !a.multi[a.selected]
			}
			return a, nil
		case key.Matches(msg, askUserKeys.Confirm):
			return a, a.confirm()
		case key.Matches(msg, askUserKeys.Cancel):
			return a, a.cancel()
		default:
			vp, cmd := a.contentViewport.Update(msg)
			a.contentViewport = vp
			return a, cmd
		}
	}
	return a, nil
}

// confirm builds the answer from the user's selection and hands it back.
func (a *askUserDialogCmp) confirm() tea.Cmd {
	q := a.req.Question
	offset := 0
	if q.AllowCustom {
		offset = 1
	}
	answer := agent.AskUserAnswer{}

	if q.MultiSelect && a.multi != nil {
		for idx, sel := range a.multi {
			if !sel {
				continue
			}
			if q.AllowCustom && idx == 0 {
				answer.Selected = append(answer.Selected, "Other")
			} else if optIdx := idx - offset; optIdx >= 0 && optIdx < len(q.Options) {
				answer.Selected = append(answer.Selected, q.Options[optIdx].Label)
			}
		}
	} else if q.AllowCustom && a.selected == 0 {
		// Custom input selected: mark "Other" so the model knows the user had
		// their own answer.
		answer.Selected = []string{"Other"}
	} else if optIdx := a.selected - offset; optIdx >= 0 && optIdx < len(q.Options) {
		answer.Selected = []string{q.Options[optIdx].Label}
	}

	return a.replyAnswer(answer)
}

// cancel sends an empty answer (the model sees "user skipped") and closes.
func (a *askUserDialogCmp) cancel() tea.Cmd {
	return a.replyAnswer(agent.AskUserAnswer{})
}

// replyAnswer writes the answer to Reply first (unblocking the runner), then
// emits the close message so the app dismisses the dialog.
func (a *askUserDialogCmp) replyAnswer(answer agent.AskUserAnswer) tea.Cmd {
	if a.req.Reply != nil {
		a.req.Reply <- answer
	}
	return util.CmdHandler(CloseAskUserMsg{})
}

func (a *askUserDialogCmp) render() string {
	t := theme.CurrentTheme()
	base := styles.BaseStyle()

	title := base.
		Bold(true).
		Width(a.width - 4).
		Foreground(t.Primary()).
		Render("Clarification")

	innerW := a.width - 4
	if innerW < 20 {
		innerW = 20
	}
	lines := renderAskUserCard(a.req.Question, a.selected, a.multi, innerW)

	a.contentViewport.SetWidth(innerW)
	a.contentViewport.SetHeight(max(1, a.height-lipgloss.Height(title)-5))
	a.contentViewport.SetContent(strings.Join(lines, "\n"))

	content := lipgloss.JoinVertical(
		lipgloss.Top,
		title,
		base.Render(strings.Repeat(" ", innerW)),
		a.styleViewport(),
	)

	return base.
		Padding(1, 0, 0, 1).
		Border(lipgloss.RoundedBorder()).
		BorderBackground(t.Background()).
		BorderForeground(t.TextMuted()).
		Width(a.width).
		Height(a.height).
		Render(content)
}

func (a *askUserDialogCmp) styleViewport() string {
	t := theme.CurrentTheme()
	return lipgloss.NewStyle().Background(t.Background()).Render(a.contentViewport.View())
}

func (a *askUserDialogCmp) View() tea.View {
	return tea.NewView(a.render())
}

func (a *askUserDialogCmp) BindingKeys() []key.Binding {
	return layout.KeyMapToSlice(askUserKeys)
}

func (a *askUserDialogCmp) SetSize() tea.Cmd {
	if a.windowSize.Width < 20 {
		a.width = 40
		a.height = 10
		return nil
	}
	a.width = int(float64(a.windowSize.Width) * 0.7)
	a.height = int(float64(a.windowSize.Height) * 0.5)
	return nil
}

func (a *askUserDialogCmp) SetQuestion(req AskUserRequest) tea.Cmd {
	a.req = req
	a.selected = 0
	a.multi = nil
	return a.SetSize()
}

func NewAskUserDialogCmp() AskUserDialogCmp {
	return &askUserDialogCmp{
		contentViewport: viewport.New(),
	}
}

// renderAskUserCard renders a clarification question card with selectable
// options. When AllowCustom is on, index 0 is the custom-text input and options
// are 1..N; otherwise options are 0..N-1. When multi is non-nil, multi-select
// mode is active: Space toggles.
func renderAskUserCard(q agent.AskUserQuestion, selected int, multi map[int]bool, width int) []string {
	t := theme.CurrentTheme()
	bg := t.Background()
	head := lipgloss.NewStyle().Background(bg).Foreground(t.Primary()).Bold(true)
	meta := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted())
	body := lipgloss.NewStyle().Background(bg).Foreground(t.Text())
	active := lipgloss.NewStyle().Background(bg).Foreground(t.Primary()).Bold(true)

	var lines []string
	lines = append(lines, head.Render("▸ "+q.Header+": "+q.Question))
	lines = append(lines, "")

	offset := 0
	if q.AllowCustom {
		offset = 1
		// Index 0: always a custom text input.
		prefix := "  "
		if selected == 0 {
			prefix = active.Render("▶") + " "
		} else {
			prefix += "  "
		}
		lines = append(lines, prefix+body.Render("💬 输入自定义回答（选中后按 Enter，在下方输入）"))
		lines = append(lines, "")
	}

	for i, opt := range q.Options {
		optIdx := i + offset // shift by 1 for the custom input when allowed
		prefix := "  "
		suffix := ""
		if q.MultiSelect && multi != nil {
			if multi[optIdx] {
				prefix += "[x] "
			} else {
				prefix += "[ ] "
			}
		} else if optIdx == selected {
			prefix = active.Render("▶") + " "
		} else {
			prefix += "  "
		}
		label := opt.Label
		if opt.Description != "" {
			suffix = meta.Render(" — " + opt.Description)
		}
		ln := prefix + body.Render(label) + suffix
		if w := runewidth.StringWidth(ln); w > width {
			ln = runewidth.Truncate(ln, width, "…")
		}
		lines = append(lines, ln)
	}

	lines = append(lines, "")
	hint := "  [↑/↓] navigate  [enter] confirm  [esc] cancel"
	if q.MultiSelect {
		hint = "  [↑/↓] navigate  [space] toggle  [enter] confirm  [esc] cancel"
	}
	lines = append(lines, meta.Render(hint))

	return lines
}
