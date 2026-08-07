package tui

import (
	"code-agent/cmd/codeagent/tui/theme"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// paletteOverlay is the slash-command menu. Unlike the other overlays it is
// always alive (m.palette, a value member) rather than open/closed: whether it
// shows is derived from the composer text (paletteActive), so it needs no open
// state — but the selection index must survive across frames, which is why the
// value persists in the model. Its Key and View need the current model because
// the menu filters on the live composer value.
type paletteOverlay struct {
	cmdIdx int // selected slash-command in the palette
}

// Key drives the command menu. Returns handled=false for keys it doesn't own
// (e.g. typing), so they fall through to the composer.
func (o *paletteOverlay) Key(msg tea.KeyMsg, m *model) (Overlay, bool, tea.Cmd) {
	cmds := filterCommands(m.composer.Value())
	o.cmdIdx = clampInt(o.cmdIdx, 0, len(cmds)-1)
	switch msg.String() {
	case "up", "ctrl+p":
		if o.cmdIdx > 0 {
			o.cmdIdx--
		}
		return o, true, nil
	case "down", "ctrl+n":
		if o.cmdIdx < len(cmds)-1 {
			o.cmdIdx++
		}
		return o, true, nil
	case "tab":
		m.composer.SetValue(cmds[o.cmdIdx].name + " ")
		o.cmdIdx = 0
		return o, true, nil
	case "esc":
		m.composer.Reset()
		m.syncComposer()
		o.cmdIdx = 0
		return o, true, nil
	case "enter":
		cmd := m.runCommand(cmds[o.cmdIdx].name, commandArgs(m.composer.Value()))
		o.cmdIdx = 0
		return o, true, cmd
	}
	return o, false, nil
}

func (o *paletteOverlay) View(width int, m *model) []string {
	cmds := filterCommands(m.composer.Value())
	return renderPalette(cmds, clampInt(o.cmdIdx, 0, len(cmds)-1), width)
}

// renderPalette renders the slash-command menu lines (shown in the live region
// just above the composer). The selected row is marked; not-yet-wired commands
// are dimmed with a hint.
func renderPalette(cmds []command, idx, width int) []string {
	lines := []string{theme.Default.Meta.Render("commands  (↑/↓ select · enter run · esc cancel)")}
	for i, c := range cmds {
		marker := "  "
		name := c.name
		if i == idx {
			marker = theme.Default.PaletteSel.Render("▌ ")
			name = theme.Default.PaletteSel.Render(name)
		}
		desc := c.desc
		if !c.ready {
			desc += " " + theme.Default.Soon.Render("(soon)")
		}
		line := marker + name + "  " + theme.Default.Meta.Render(desc)
		lines = append(lines, lipgloss.NewStyle().MaxWidth(width).Render(line))
	}
	return lines
}
