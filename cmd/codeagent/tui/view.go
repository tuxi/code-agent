package tui

import (
	"code-agent/cmd/codeagent/tui/theme"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
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
		return []string{theme.Default.Meta.Render(compactionLine(e))}
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
	mark := theme.Default.OK.Render("✓")
	switch e.Status {
	case StatusFail:
		mark = theme.Default.Fail.Render("✗")
	case StatusPending:
		mark = theme.Default.Meta.Render("◦")
	}
	parts := []string{}
	if a := briefArgs(e.Args); a != "" {
		parts = append(parts, theme.Default.Args.Render(a))
	}
	if e.Status == StatusFail && e.Failure != "" {
		parts = append(parts, theme.Default.Fail.Render(e.Failure))
	}
	if d := e.Duration(); d >= 500*time.Millisecond {
		parts = append(parts, theme.Default.Meta.Render(fmt.Sprintf("(%.1fs)", d.Seconds())))
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
	head := theme.Default.OK.Render("✓") + " " + collapsedLabel(e.Name, len(e.Children))
	lines := []string{head}
	for _, c := range e.Children {
		label := c.Name
		if a := briefArgs(c.Args); a != "" {
			label = a
		}
		lines = append(lines, "  "+theme.Default.Body.Render(runewidth.Truncate(label, width-2, "…")))
	}
	return lines
}

func entrySkill(e Item) []string {
	label := "◆ skill " + e.Name
	if e.Version != "" {
		label += " v" + e.Version
	}
	return []string{theme.Default.Skill.Render(label)}
}

func entryReflection(e Item, width int) []string {
	lines := []string{theme.Default.Reflection.Render("↻ reflection")}
	for _, ln := range wrapProse(e.Text, width-2) {
		lines = append(lines, "  "+theme.Default.Reflection.Render(ln))
	}
	return lines
}

func entryAssistant(e Item, width int) []string {
	lines := []string{theme.Default.Assistant.Render("⏺ assistant")}
	for _, ln := range wrapProse(e.Text, width) {
		lines = append(lines, theme.Default.Assistant.Render(ln))
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
		lines = append(lines, theme.Default.User.Render(prefix+ln))
	}
	return lines
}

func entryThinking(e Item, width int) []string {
	out := []string{}
	for _, ln := range wrapProse(e.Text, width) {
		out = append(out, theme.Default.Thinking.Render(ln))
	}
	return out
}

func entrySystem(e Item, width int) []string {
	var out []string
	for _, ln := range strings.Split(strings.TrimRight(e.Text, "\n"), "\n") {
		out = append(out, theme.Default.Meta.Render(runewidth.Truncate(ln, width, "…")))
	}
	return out
}

func compactionLine(e Item) string {
	switch {
	case e.Pruned:
		return fmt.Sprintf("⤳ context pruned — ~%d tokens of old tool output/reasoning dropped (no LLM call)", e.Saved)
	case e.Pending:
		// The reclaimed size is a measurement, not an assumption — it arrives with
		// the next model call. Rendering the zero values here read as "compacted
		// to 0 tokens (saved 0)", which is exactly wrong.
		return fmt.Sprintf("⤳ context compacted — %d tokens → summary %d chars (new size measured on next call)",
			e.Before, e.SummaryChars)
	case e.Ineffective:
		return fmt.Sprintf("⤳ compaction ineffective — %d→%d tokens, still over the compact threshold; cooling down (context likely exceeds the model window)",
			e.Before, e.After)
	default:
		return fmt.Sprintf("⤳ context compacted — %d→%d tokens (saved %d, summary %d chars)",
			e.Before, e.After, e.Saved, e.SummaryChars)
	}
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
		out[i] = "  " + theme.Default.Body.Render(runewidth.Truncate(ln, width-2, "…"))
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
