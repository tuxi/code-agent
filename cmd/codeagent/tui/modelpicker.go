package tui

import (
	"code-agent/cmd/codeagent/tui/theme"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// modelPickerOverlay is the /use overlay: a navigable list of configured models
// in the live region.
type modelPickerOverlay struct {
	models []modelInfo
	idx    int
}

// Key drives the /use picker. Same routing contract as the session picker: the
// global keys yield to the model, everything else is the picker's, and enter
// switches via the CURRENT model (m.useModel).
func (o *modelPickerOverlay) Key(msg tea.KeyMsg, m *model) (Overlay, bool, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if o.idx > 0 {
			o.idx--
		}
		return o, true, nil
	case "down", "j":
		if o.idx < len(o.models)-1 {
			o.idx++
		}
		return o, true, nil
	case "enter":
		if len(o.models) == 0 {
			return nil, true, nil
		}
		return nil, true, m.useModel(o.models[o.idx].name)
	case "esc":
		return nil, true, nil
	case "ctrl+c", "ctrl+z", "ctrl+o", "ctrl+p":
		return o, false, nil
	}
	return o, true, nil
}

func (o *modelPickerOverlay) View(width int, _ *model) []string {
	return renderModelPicker(o, width)
}

// renderModelPicker renders the model list for the live region, the selected
// one marked with ❯.
func renderModelPicker(p *modelPickerOverlay, width int) []string {
	lines := []string{theme.Default.Meta.Render("switch model  (↑/↓ select · enter confirm · esc cancel)")}
	for i, mi := range p.models {
		cursor, ts := "  ", theme.Default.Meta
		if i == p.idx {
			cursor, ts = theme.Default.PaletteSel.Render("❯ "), theme.Default.PaletteSel
		}
		label := mi.display
		if label == "" {
			label = mi.name
		}
		lines = append(lines, cursor+ts.Render(runewidth.Truncate(label, width-2, "…")))
	}
	return lines
}
