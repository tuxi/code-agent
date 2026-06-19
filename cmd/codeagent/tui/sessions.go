package tui

import (
	"fmt"
	"strings"
	"time"

	"code-agent/internal/session"
	"github.com/mattn/go-runewidth"
)

// sessionPicker is the /resume overlay: a navigable list of saved sessions. It
// lives in the live region (re-rendered), so ↑/↓ selection works without
// touching scrollback.
type sessionPicker struct {
	metas []session.Meta
	idx   int
}

const maxPickerItems = 8 // window the list so it never overflows the live region

// renderPicker renders the session list for the live region: each session is a
// title line (the first user message) plus a dim metadata line, the selected one
// marked with ❯.
func renderPicker(p sessionPicker, width int) []string {
	lines := []string{styleMeta.Render("resume a session  (↑/↓ select · enter resume · esc cancel)")}
	if len(p.metas) == 0 {
		return append(lines, styleMeta.Render("  no saved sessions"))
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
		lines = append(lines, styleMeta.Render(fmt.Sprintf("  … %d earlier", start)))
	}
	for i := start; i < end; i++ {
		meta := p.metas[i]
		title := sessionTitle(meta.Title)
		if title == "" {
			title = meta.ID
		}
		cursor, ts := "  ", styleAssistant
		if i == p.idx {
			cursor, ts = stylePaletteSel.Render("❯ "), stylePaletteSel
		}
		lines = append(lines, cursor+ts.Render(runewidth.Truncate(title, width-2, "…")))
		meta2 := fmt.Sprintf("    %s · %s · %d msgs", humanAgo(meta.UpdatedAt), meta.Model, meta.MessageCount)
		lines = append(lines, styleMeta.Render(runewidth.Truncate(meta2, width, "…")))
	}
	if end < len(p.metas) {
		lines = append(lines, styleMeta.Render(fmt.Sprintf("  … %d more", len(p.metas)-end)))
	}
	return lines
}

// formatSessionList is the text output for the /sessions command (printed to
// scrollback), built from the same metas the picker uses.
func formatSessionList(metas []session.Meta) string {
	if len(metas) == 0 {
		return "no saved sessions"
	}
	var b strings.Builder
	for _, m := range metas {
		t := sessionTitle(m.Title)
		if t == "" {
			t = m.ID
		}
		fmt.Fprintf(&b, "%s — %s · %d msgs · %s\n", t, m.Model, m.MessageCount, humanAgo(m.UpdatedAt))
	}
	return strings.TrimRight(b.String(), "\n")
}

// sessionTitle flattens a first-message into a single clean line.
func sessionTitle(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// humanAgo renders a coarse relative time ("18 minutes ago").
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return ago(int(d.Seconds()), "second")
	case d < time.Hour:
		return ago(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return ago(int(d.Hours()), "hour")
	default:
		return ago(int(d.Hours()/24), "day")
	}
}

func ago(n int, unit string) string {
	if n <= 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

// --- /use model picker --------------------------------------------------

type modelPicker struct {
	models []modelInfo
	idx    int
}

func renderModelPicker(p modelPicker, width int) []string {
	lines := []string{styleMeta.Render("switch model  (↑/↓ select · enter confirm · esc cancel)")}
	for i, m := range p.models {
		cursor, ts := "  ", styleMeta
		if i == p.idx {
			cursor, ts = stylePaletteSel.Render("❯ "), stylePaletteSel
		}
		lines = append(lines, cursor+ts.Render(runewidth.Truncate(m.name, width-2, "…")))
	}
	return lines
}
