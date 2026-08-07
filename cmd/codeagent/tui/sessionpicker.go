package tui

import (
	"fmt"

	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/internal/session"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// sessionPickerOverlay is the /resume overlay: a navigable list of saved
// sessions in the live region, so ↑/↓ selection works without touching
// scrollback.
type sessionPickerOverlay struct {
	metas []session.Meta
	idx   int
}

const maxPickerItems = 8 // window the list so it never overflows the live region

// Key drives the /resume picker. The global keys (ctrl+c/z/o/p) yield to the
// model's own handlers — quit/suspend/thinking-toggle/plan-toggle — the same
// routing the pre-overlay Update had, where the global switch ran before the
// picker handlers. Everything else is the picker's: navigate, enter resumes
// via the CURRENT model (never a captured receiver), esc cancels.
func (o *sessionPickerOverlay) Key(msg tea.KeyMsg, m *model) (Overlay, bool, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if o.idx > 0 {
			o.idx--
		}
		return o, true, nil
	case "down", "j":
		if o.idx < len(o.metas)-1 {
			o.idx++
		}
		return o, true, nil
	case "enter":
		if len(o.metas) == 0 {
			return nil, true, nil
		}
		return nil, true, m.resume(o.metas[o.idx])
	case "esc":
		return nil, true, nil
	case "ctrl+c", "ctrl+z", "ctrl+o", "ctrl+p":
		return o, false, nil
	}
	return o, true, nil
}

func (o *sessionPickerOverlay) View(width int, _ *model) []string {
	return renderPicker(o, width)
}

// renderPicker renders the session list for the live region: each session is a
// title line (the first user message) plus a dim metadata line, the selected one
// marked with ❯.
func renderPicker(p *sessionPickerOverlay, width int) []string {
	lines := []string{theme.Default.Meta.Render("resume a session  (↑/↓ select · enter resume · esc cancel)")}
	if len(p.metas) == 0 {
		return append(lines, theme.Default.Meta.Render("  no saved sessions"))
	}

	start := 0
	if len(p.metas) > maxPickerItems {
		start = clampInt(p.idx-maxPickerItems/2, 0, len(p.metas)-maxPickerItems)
	}
	end := start + maxPickerItems
	if end > len(p.metas) {
		end = len(p.metas)
	}
	if start > 0 {
		lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  … %d earlier", start)))
	}
	for i := start; i < end; i++ {
		meta := p.metas[i]
		title := effectiveTitle(meta)
		if title == "" {
			title = meta.ID
		}
		cursor, ts := "  ", theme.Default.Assistant
		if i == p.idx {
			cursor, ts = theme.Default.PaletteSel.Render("❯ "), theme.Default.PaletteSel
		}
		lines = append(lines, cursor+ts.Render(runewidth.Truncate(title, width-2, "…")))
		meta2 := fmt.Sprintf("    %s · %s · %d msgs", humanAgo(meta.UpdatedAt), meta.Model, meta.MessageCount)
		lines = append(lines, theme.Default.Meta.Render(runewidth.Truncate(meta2, width, "…")))
	}
	if end < len(p.metas) {
		lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  … %d more", len(p.metas)-end)))
	}
	return lines
}
