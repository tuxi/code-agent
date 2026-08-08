package dialog

import (
	"fmt"
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

// ClosePlanApprovalMsg tells the app the plan dialog answered so the app can
// close it. The decision itself has already been delivered on the request's
// Reply channel before this message is emitted.
type ClosePlanApprovalMsg struct{}

// PlanApprovalRequest is the dialog's view of a pending plan approval: the
// proposed plan and the channel the runner goroutine blocks on until the user
// decides.
type PlanApprovalRequest struct {
	Plan  agent.Plan
	Reply chan agent.PlanDecision
}

// PlanApprovalDialogCmp is the plan approval dialog.
type PlanApprovalDialogCmp interface {
	tea.Model
	layout.Bindings
	SetPlan(req PlanApprovalRequest) tea.Cmd
}

type planKeyMap struct {
	Approve key.Binding
	Reject  key.Binding
}

var planKeys = planKeyMap{
	Approve: key.NewBinding(
		key.WithKeys("a", "A", "enter"),
		key.WithHelp("a/enter", "approve"),
	),
	Reject: key.NewBinding(
		key.WithKeys("r", "R", "esc", "ctrl+c"),
		key.WithHelp("r/esc", "reject"),
	),
}

// planApprovalDialogCmp is the plan-approval dialog. It answers the request
// itself — writes the decision to PlanApprovalRequest.Reply (unblocking the
// runner goroutine) — then emits ClosePlanApprovalMsg so the app can close it.
// The app must not resend the decision.
type planApprovalDialogCmp struct {
	width           int
	height          int
	req             PlanApprovalRequest
	windowSize      tea.WindowSizeMsg
	contentViewport viewport.Model
}

func (p *planApprovalDialogCmp) Init() tea.Cmd {
	return p.contentViewport.Init()
}

func (p *planApprovalDialogCmp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.windowSize = msg
		return p, p.SetSize()
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, planKeys.Approve):
			return p, p.respond(agent.PlanApproved)
		case key.Matches(msg, planKeys.Reject):
			return p, p.respond(agent.PlanRejected)
		default:
			vp, cmd := p.contentViewport.Update(msg)
			p.contentViewport = vp
			return p, cmd
		}
	}
	return p, nil
}

// respond answers the request: writes the decision to Reply first (unblocking
// the runner), then emits the close message so the app dismisses the dialog.
func (p *planApprovalDialogCmp) respond(decision agent.PlanDecision) tea.Cmd {
	if p.req.Reply != nil {
		p.req.Reply <- decision
	}
	return util.CmdHandler(ClosePlanApprovalMsg{})
}

func (p *planApprovalDialogCmp) render() string {
	t := theme.CurrentTheme()
	base := styles.BaseStyle()

	title := base.
		Bold(true).
		Width(p.width - 4).
		Foreground(t.Primary()).
		Render("Plan Approval")

	innerW := p.width - 4
	if innerW < 20 {
		innerW = 20
	}
	lines := renderPlanApprovalCard(p.req.Plan, innerW)

	p.contentViewport.SetWidth(innerW)
	p.contentViewport.SetHeight(max(1, p.height-lipgloss.Height(title)-5))
	p.contentViewport.SetContent(strings.Join(lines, "\n"))

	content := lipgloss.JoinVertical(
		lipgloss.Top,
		title,
		base.Render(strings.Repeat(" ", innerW)),
		p.styleViewport(),
	)

	return base.
		Padding(1, 0, 0, 1).
		Border(lipgloss.RoundedBorder()).
		BorderBackground(t.Background()).
		BorderForeground(t.TextMuted()).
		Width(p.width).
		Height(p.height).
		Render(content)
}

func (p *planApprovalDialogCmp) styleViewport() string {
	t := theme.CurrentTheme()
	return lipgloss.NewStyle().Background(t.Background()).Render(p.contentViewport.View())
}

func (p *planApprovalDialogCmp) View() tea.View {
	return tea.NewView(p.render())
}

func (p *planApprovalDialogCmp) BindingKeys() []key.Binding {
	return layout.KeyMapToSlice(planKeys)
}

func (p *planApprovalDialogCmp) SetSize() tea.Cmd {
	if p.windowSize.Width < 20 {
		p.width = 40
		p.height = 10
		return nil
	}
	// Plans are long-form text: take most of the window.
	p.width = int(float64(p.windowSize.Width) * 0.8)
	p.height = int(float64(p.windowSize.Height) * 0.8)
	return nil
}

func (p *planApprovalDialogCmp) SetPlan(req PlanApprovalRequest) tea.Cmd {
	p.req = req
	return p.SetSize()
}

func NewPlanApprovalDialogCmp() PlanApprovalDialogCmp {
	return &planApprovalDialogCmp{
		contentViewport: viewport.New(),
	}
}

// renderPlanApprovalCard renders the plan card body: the plan title + metadata
// and a preview of the plan content. The dialog's own border wraps it; no inner
// box is drawn.
func renderPlanApprovalCard(plan agent.Plan, width int) []string {
	t := theme.CurrentTheme()
	bg := t.Background()
	head := lipgloss.NewStyle().Background(bg).Foreground(t.Primary()).Bold(true)
	meta := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted())
	body := lipgloss.NewStyle().Background(bg).Foreground(t.Text())

	var lines []string
	lines = append(lines, head.Render("▸ Plan: "+plan.Title))
	lines = append(lines, meta.Render(fmt.Sprintf("  ID: %s  |  Saved: %s", plan.ID, plan.FilePath)))
	lines = append(lines, "")

	// Preview the first ~15 lines of the plan content.
	contentLines := strings.Split(plan.Content, "\n")
	maxLines := 15
	if len(contentLines) < maxLines {
		maxLines = len(contentLines)
	}
	for i := 0; i < maxLines; i++ {
		ln := runewidth.Truncate(contentLines[i], width-2, "…")
		lines = append(lines, body.Render("  "+ln))
	}
	if len(contentLines) > maxLines {
		lines = append(lines, meta.Render(fmt.Sprintf("  … %d more lines", len(contentLines)-maxLines)))
	}
	lines = append(lines, "")
	lines = append(lines, meta.Render("  [a] approve  [r] reject  (esc/r to reject)"))

	return lines
}
