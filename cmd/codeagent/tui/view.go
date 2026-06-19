package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Styles. Adaptive colors adjust to light/dark terminals; lipgloss honors
// NO_COLOR automatically, so this stays accessible without a config knob.
var (
	accent = lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafff"}

	styleUser       = lipgloss.NewStyle().Bold(true).Foreground(accent)
	styleThinking   = lipgloss.NewStyle().Faint(true).Italic(true)
	styleOK         = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#207020", Dark: "#5fd75f"})
	styleFail       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#af0000", Dark: "#ff5f5f"})
	styleSkill      = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#8700af", Dark: "#d787ff"})
	styleReflection = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#af5f00", Dark: "#ffaf5f"})
	styleMeta       = lipgloss.NewStyle().Faint(true)
	styleArgs       = lipgloss.NewStyle().Faint(true)
	styleAssistant  = lipgloss.NewStyle()
	styleAsstLabel  = lipgloss.NewStyle().Bold(true).Foreground(accent)
	styleBody       = lipgloss.NewStyle().Faint(true)
	stylePaletteSel = lipgloss.NewStyle().Bold(true).Foreground(accent)
	styleSoon       = lipgloss.NewStyle().Faint(true).Italic(true)
	styleApproveBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(styleFail.GetForeground())
	styleApproveHl  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafff"})
	styleApproveDim = lipgloss.NewStyle().Faint(true)
)

const failBodyLines = 12 // a failed tool prints this many body lines (the failure is the signal)

// renderEntry formats one timeline item as the lines printed to scrollback. There
// is no in-place expand in inline mode (scrollback is immutable), so the body
// shown is a print-time decision: a failure prints its body, a success prints
// just its header (the agent's reply carries what it found).
func renderEntry(e Item, width int) []string {
	if width < 8 {
		width = 8
	}
	switch e.Kind {
	case ItemUser:
		return entryUser(e, width)
	case ItemThinking:
		return entryThinking(e, width)
	case ItemTool:
		if len(e.Children) > 0 {
			return entryGroup(e, width)
		}
		return entryTool(e, width)
	case ItemSkill:
		return entrySkill(e)
	case ItemReflection:
		return entryReflection(e, width)
	case ItemCompaction:
		return []string{styleMeta.Render(compactionLine(e))}
	case ItemAssistant:
		return entryAssistant(e, width)
	case ItemSystem:
		return entrySystem(e, width)
	}
	return nil
}

func entryTool(e Item, width int) []string {
	lines := []string{toolHeader(e)}
	show := e.Status == StatusFail || mutationTools[e.Name]
	if show && strings.TrimSpace(e.Text) != "" {
		lines = append(lines, indentBody(e.Text, width, failBodyLines)...)
	}
	return lines
}

func toolHeader(e Item) string {
	mark := styleOK.Render("✓")
	switch e.Status {
	case StatusFail:
		mark = styleFail.Render("✗")
	case StatusPending:
		mark = styleMeta.Render("◦")
	}
	parts := []string{}
	if a := briefArgs(e.Args); a != "" {
		parts = append(parts, styleArgs.Render(a))
	}
	if e.Status == StatusFail && e.Failure != "" {
		parts = append(parts, styleFail.Render(e.Failure))
	}
	if d := e.Duration(); d >= 500*time.Millisecond {
		parts = append(parts, styleMeta.Render(fmt.Sprintf("(%.1fs)", d.Seconds())))
	}
	line := mark + " " + e.Name
	if len(parts) > 0 {
		line += "  " + strings.Join(parts, "  ")
	}
	return line
}

// entryGroup renders a collapsed run (kept for the projection library / future
// turn-end batch collapse; live printing does not collapse).
func entryGroup(e Item, width int) []string {
	head := styleOK.Render("✓") + " " + collapsedLabel(e.Name, len(e.Children))
	lines := []string{head}
	for _, c := range e.Children {
		label := c.Name
		if a := briefArgs(c.Args); a != "" {
			label = a
		}
		lines = append(lines, "  "+styleBody.Render(runewidth.Truncate(label, width-2, "…")))
	}
	return lines
}

func entrySkill(e Item) []string {
	label := "◆ skill " + e.Name
	if e.Version != "" {
		label += " v" + e.Version
	}
	return []string{styleSkill.Render(label)}
}

func entryReflection(e Item, width int) []string {
	lines := []string{styleReflection.Render("↻ reflection")}
	for _, ln := range wrapProse(e.Text, width-2) {
		lines = append(lines, "  "+styleReflection.Render(ln))
	}
	return lines
}

func entryAssistant(e Item, width int) []string {
	lines := []string{styleAsstLabel.Render("⏺ assistant")}
	for _, ln := range wrapProse(e.Text, width) {
		lines = append(lines, styleAssistant.Render(ln))
	}
	return lines
}

func entryUser(e Item, width int) []string {
	var lines []string
	for i, ln := range wrapProse(strings.TrimRight(e.Text, "\n"), width-2) {
		prefix := "  "
		if i == 0 {
			prefix = "› "
		}
		lines = append(lines, styleUser.Render(prefix+ln))
	}
	return lines
}

func entryThinking(e Item, width int) []string {
	out := []string{}
	for _, ln := range wrapProse(e.Text, width) {
		out = append(out, styleThinking.Render(ln))
	}
	return out
}

func entrySystem(e Item, width int) []string {
	var out []string
	for _, ln := range strings.Split(strings.TrimRight(e.Text, "\n"), "\n") {
		out = append(out, styleMeta.Render(runewidth.Truncate(ln, width, "…")))
	}
	return out
}

func compactionLine(e Item) string {
	return fmt.Sprintf("⤳ context compacted — %d→%d tokens (saved %d, summary %d chars)",
		e.Before, e.After, e.Saved, e.SummaryChars)
}

// renderPalette renders the slash-command menu lines (shown in the live region
// just above the composer). The selected row is marked; not-yet-wired commands
// are dimmed with a hint.
func renderPalette(cmds []command, idx, width int) []string {
	lines := []string{styleMeta.Render("commands  (↑/↓ select · enter run · esc cancel)")}
	for i, c := range cmds {
		marker := "  "
		name := c.name
		if i == idx {
			marker = stylePaletteSel.Render("▌ ")
			name = stylePaletteSel.Render(c.name)
		}
		desc := c.desc
		if !c.ready {
			desc += "  " + styleSoon.Render("(soon)")
		}
		line := marker + name + "  " + styleMeta.Render(desc)
		lines = append(lines, lipgloss.NewStyle().MaxWidth(width).Render(line))
	}
	return lines
}

// indentBody renders a tool result body: each original line clipped to width
// (logs/code keep their own line structure), indented and dimmed, capped at max.
func indentBody(text string, width, max int) []string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > max {
		extra := len(lines) - max
		lines = append(lines[:max:max], fmt.Sprintf("… (%d more lines)", extra))
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = "  " + styleBody.Render(runewidth.Truncate(ln, width-2, "…"))
	}
	return out
}

// collapsedLabel gives a few common tools a friendlier plural.
func collapsedLabel(tool string, n int) string {
	switch tool {
	case "read_file":
		return fmt.Sprintf("Read %d files", n)
	case "list_files":
		return fmt.Sprintf("Listed %d directories", n)
	case "grep":
		return fmt.Sprintf("Searched %d times", n)
	case "run_command":
		return fmt.Sprintf("Ran %d commands", n)
	default:
		return fmt.Sprintf("%s ×%d", tool, n)
	}
}

// briefArgs renders tool arguments as a short, single-line hint.
func briefArgs(args string) string {
	args = strings.TrimSpace(args)
	if args == "" || args == "{}" {
		return ""
	}
	return runewidth.Truncate(strings.Join(strings.Fields(args), " "), 72, "…")
}

// wrapProse word-wraps prose to a display width (runewidth-aware, so CJK counts
// as two columns), preserving blank lines between paragraphs.
func wrapProse(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	var out []string
	for _, para := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line, lw := "", 0
		for _, w := range words {
			ww := runewidth.StringWidth(w)
			switch {
			case lw == 0:
				line, lw = w, ww
			case lw+1+ww > width:
				out = append(out, line)
				line, lw = w, ww
			default:
				line += " " + w
				lw += 1 + ww
			}
		}
		out = append(out, line)
	}
	return out
}

func humanK(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// --- approval card (M3) --------------------------------------------------

const maxPreviewLines = 8 // max rows shown per argument in the preview

// renderApprovalCard renders the approval dialog in the live region: a bordered
// card showing the tool name + a readable argument preview, plus the y/n
// selector the user navigates with ↑/↓ and confirms with Enter.
func renderApprovalCard(req approvalReq, approveIdx, width int) []string {
	innerW := width - 4 // border takes 2 on each side
	if innerW < 20 {
		innerW = 20
	}
	preview := approvalPreview(req.tool, string(req.input), innerW)
	sel := approvalSelector(approveIdx)
	lines := append(preview, "", sel)
	return strings.Split(styleApproveBox.Width(innerW).Render(strings.Join(lines, "\n")), "\n")
}

// approvalPreview renders the tool input as readable key-value lines so the user
// can see what they're approving — the REPl's per-field display, now in the TUI.
func approvalPreview(tool string, input string, width int) []string {
	var raw map[string]any
	json.Unmarshal([]byte(input), &raw)

	var lines []string
	// The tool name gets a bold header line.
	lines = append(lines, styleFail.Render("▸ run "+tool+"?"))

	// Ordered fields: show the primary ones first, then the rest.
	for _, key := range []string{"command", "path", "old", "new", "patch", "message"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		s := fmt.Sprint(v)
		if s == "" {
			lines = append(lines, styleMeta.Render(fmt.Sprintf("  %s: (empty)", key)))
		} else {
			lines = append(lines, styleMeta.Render(fmt.Sprintf("  %s:", key)))
			for _, ln := range previewLines(s, width-4, maxPreviewLines) {
				lines = append(lines, styleBody.Render("    "+ln))
			}
		}
		delete(raw, key)
	}
	// Remaining fields (if any) in alphabetical order.
	var rest []string
	for k := range raw {
		rest = append(rest, k)
	}
	// The keys are arbitrary so order doesn't matter; just don't skip useful info.
	for _, k := range rest {
		s := fmt.Sprint(raw[k])
		if s == "" {
			continue
		}
		lines = append(lines, styleMeta.Render(fmt.Sprintf("  %s:", k)))
		for _, ln := range previewLines(s, width-4, 4) {
			lines = append(lines, styleBody.Render("    "+ln))
		}
	}
	return lines
}

func previewLines(s string, width, max int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > max {
		extra := len(lines) - max
		lines = append(lines[:max:max], styleMeta.Render(fmt.Sprintf("  … %d more lines", extra)))
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = runewidth.Truncate(ln, width, "…")
	}
	return out
}

// approvalSelector shows y/n with the active choice highlighted — the ↑/↓
// movable cursor the approveIdx field drives.
func approvalSelector(idx int) string {
	y, n := " ", " "
	if idx == 0 {
		y, n = styleApproveHl.Render("▶ [y]"), styleApproveDim.Render("  [n]")
	} else {
		y, n = styleApproveDim.Render("  [y]"), styleApproveHl.Render("▶ [n]")
	}
	return fmt.Sprintf("%s approve  %s deny  %s", y, n, styleMeta.Render("(↑/↓ select · enter confirm · esc cancel)"))
}
