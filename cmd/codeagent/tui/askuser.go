package tui

import (
	"strings"

	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// askUserOverlay is the clarification card: the question, a custom-text option,
// and the model's suggested answers. The runner goroutine blocks on reply until
// the user answers (or cancels).
type askUserOverlay struct {
	q        agent.AskUserQuestion
	reply    chan agent.AskUserAnswer
	selected int          // highlighted option index (↑/↓)
	multi    map[int]bool // selected indices when multiSelect is on
}

// Key drives the ask_user card. Index 0 is always the custom-text input;
// indices 1..N map to q.Options[0..N-1]. ↑/↓ navigates, Space toggles
// multi-select, Enter confirms, Esc cancels. Modal like the other cards — the
// runner is waiting.
func (o *askUserOverlay) Key(msg tea.KeyMsg, _ *model) (Overlay, bool, tea.Cmd) {
	optCount := len(o.q.Options) + 1 // +1 for the always-present custom input

	switch msg.String() {
	case "up", "k", "ctrl+p":
		if o.selected > 0 {
			o.selected--
		}
	case "down", "j", "ctrl+n":
		if o.selected < optCount-1 {
			o.selected++
		}
	case " ":
		if o.q.MultiSelect {
			if o.multi == nil {
				o.multi = make(map[int]bool)
			}
			o.multi[o.selected] = !o.multi[o.selected]
		}
	case "enter":
		o.confirm()
		return nil, true, nil
	case "esc", "escape", "ctrl+c":
		// Cancel: send empty answer (model gets "user skipped").
		o.reply <- agent.AskUserAnswer{}
		return nil, true, nil
	}
	return o, true, nil
}

// confirm builds the answer from the user's selection and hands it back.
// Index 0 = custom text input, 1..N = q.Options[0..N-1].
func (o *askUserOverlay) confirm() {
	q := o.q
	answer := agent.AskUserAnswer{}

	if q.MultiSelect && o.multi != nil {
		for idx, sel := range o.multi {
			if sel {
				if idx == 0 {
					answer.Selected = append(answer.Selected, "Other")
				} else if optIdx := idx - 1; optIdx < len(q.Options) {
					answer.Selected = append(answer.Selected, q.Options[optIdx].Label)
				}
			}
		}
	} else if o.selected == 0 {
		// Custom input selected: mark "Other" so the model knows the user had
		// their own answer (free-text notes come from the composer).
		answer.Selected = []string{"Other"}
	} else if optIdx := o.selected - 1; optIdx < len(q.Options) {
		answer.Selected = []string{q.Options[optIdx].Label}
	}

	o.reply <- answer
}

func (o *askUserOverlay) View(width int, _ *model) []string {
	return renderAskUserCard(o.q, o.selected, o.multi, width)
}

// renderAskUserCard renders a clarification question card with selectable
// options in the live region. selected is the currently highlighted option index:
// 0 = custom text input (always shown), 1..N = q.Options[0..N-1].
// When multi is non-nil, multi-select mode is active: Space toggles.
func renderAskUserCard(q agent.AskUserQuestion, selected int, multi map[int]bool, width int) []string {
	innerW := width - 4
	if innerW < 20 {
		innerW = 20
	}

	var lines []string
	lines = append(lines, theme.Default.Skill.Render("▸ "+q.Header+": "+q.Question))
	lines = append(lines, "")

	// Index 0: always a custom text input.
	{
		prefix := "  "
		if selected == 0 {
			prefix = theme.Default.ApproveBox().Render("▶") + " "
		} else {
			prefix += "  "
		}
		lines = append(lines, prefix+theme.Default.Body.Render("💬 输入自定义回答（选中后按 Enter，在下方输入）"))
		lines = append(lines, "")
	}

	for i, opt := range q.Options {
		optIdx := i + 1 // shift by 1 for the custom input
		prefix := "  "
		suffix := ""
		if q.MultiSelect && multi != nil {
			if multi[optIdx] {
				prefix += "[x] "
			} else {
				prefix += "[ ] "
			}
		} else if optIdx == selected {
			prefix = theme.Default.ApproveBox().Render("▶") + " "
		} else {
			prefix += "  "
		}
		label := opt.Label
		if opt.Description != "" {
			suffix = theme.Default.Meta.Render(" — " + opt.Description)
		}
		ln := prefix + theme.Default.Body.Render(label) + suffix
		if w := runewidth.StringWidth(ln); w > innerW {
			ln = runewidth.Truncate(ln, innerW, "…")
		}
		lines = append(lines, ln)
	}

	lines = append(lines, "")
	hint := "  [↑/↓] navigate  [enter] confirm  [esc] cancel"
	if q.MultiSelect {
		hint = "  [↑/↓] navigate  [space] toggle  [enter] confirm  [esc] cancel"
	}
	lines = append(lines, theme.Default.Meta.Render(hint))

	return strings.Split(theme.Default.ApproveBox().Width(innerW).Render(strings.Join(lines, "\n")), "\n")
}
