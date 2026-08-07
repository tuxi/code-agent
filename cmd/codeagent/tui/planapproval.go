package tui

import (
	"fmt"
	"strings"

	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// planOverlay is the plan-approval card: the proposed plan, [a] approve /
// [r] reject. The runner goroutine blocks on reply until the user answers.
type planOverlay struct {
	plan  agent.Plan
	reply chan agent.PlanDecision
}

// Key drives the plan approval card: a approves, r rejects (esc/ctrl+c too).
// Modal like the approval card — the runner is waiting on the plan decision.
func (o *planOverlay) Key(msg tea.KeyMsg, _ *model) (Overlay, bool, tea.Cmd) {
	switch msg.String() {
	case "a", "A", "enter":
		o.reply <- agent.PlanApproved
		return nil, true, nil
	case "r", "R", "esc", "ctrl+c":
		o.reply <- agent.PlanRejected
		return nil, true, nil
	}
	return o, true, nil
}

func (o *planOverlay) View(width int, _ *model) []string {
	return renderPlanApprovalCard(o.plan, width)
}

// renderPlanApprovalCard renders a plan for user approval in the live region.
func renderPlanApprovalCard(plan agent.Plan, width int) []string {
	innerW := width - 4
	if innerW < 20 {
		innerW = 20
	}

	var lines []string
	lines = append(lines, theme.Default.Skill.Render("▸ Plan: "+plan.Title))
	lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  ID: %s  |  Saved: %s", plan.ID, plan.FilePath)))
	lines = append(lines, "")

	// Preview first ~15 lines of the plan content.
	contentLines := strings.Split(plan.Content, "\n")
	maxLines := 15
	if len(contentLines) < maxLines {
		maxLines = len(contentLines)
	}
	for i := 0; i < maxLines; i++ {
		ln := runewidth.Truncate(contentLines[i], innerW-2, "…")
		lines = append(lines, theme.Default.Body.Render("  "+ln))
	}
	if len(contentLines) > maxLines {
		lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  … %d more lines", len(contentLines)-maxLines)))
	}
	lines = append(lines, "")
	lines = append(lines, theme.Default.Meta.Render("  [a] approve  [r] reject  (esc/r to reject)"))

	return strings.Split(theme.Default.ApproveBox().Width(innerW).Render(strings.Join(lines, "\n")), "\n")
}
