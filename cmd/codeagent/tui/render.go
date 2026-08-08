package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"code-agent/cmd/codeagent/tui/theme"
)

const failBodyLines = 12 // a failed tool prints this many body lines (the failure is the signal)

// mutationTools are the tools whose output the user always wants to see, even on
// success — what was edited/created/applied/committed is the primary signal.
var mutationTools = map[string]bool{
	"edit_file":   true,
	"create_file": true,
	"apply_patch": true,
	"git_commit":  true,
}

// entryStyles bundles the themed styles the entry renderers use. The old code
// read ready-made styles off the deleted default theme singleton; the theme now
// exposes only colors, so each style is assembled from the color accessors on
// every call — the active theme can be switched at runtime.
type entryStyles struct {
	meta, ok, fail, body, args, skill, reflection, assistant, user, thinking lipgloss.Style
}

func curEntryStyles() entryStyles {
	t := theme.CurrentTheme()
	return entryStyles{
		meta:       lipgloss.NewStyle().Faint(true).Foreground(t.TextMuted()),
		ok:         lipgloss.NewStyle().Foreground(t.Success()),
		fail:       lipgloss.NewStyle().Bold(true).Foreground(t.Error()),
		body:       lipgloss.NewStyle().Faint(true).Foreground(t.TextMuted()),
		args:       lipgloss.NewStyle().Faint(true).Foreground(t.TextMuted()),
		skill:      lipgloss.NewStyle().Foreground(t.Accent()),
		reflection: lipgloss.NewStyle().Foreground(t.Warning()),
		assistant:  lipgloss.NewStyle().Foreground(t.Text()),
		user:       lipgloss.NewStyle().Bold(true).Foreground(t.Secondary()),
		thinking:   lipgloss.NewStyle().Faint(true).Italic(true).Foreground(t.TextMuted()),
	}
}

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
		s := curEntryStyles()
		return []string{s.meta.Render(compactionLine(e))}
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
	s := curEntryStyles()
	mark := s.ok.Render("✓")
	switch e.Status {
	case StatusFail:
		mark = s.fail.Render("✗")
	case StatusPending:
		mark = s.meta.Render("◦")
	}
	parts := []string{}
	if a := briefArgs(e.Args); a != "" {
		parts = append(parts, s.args.Render(a))
	}
	if e.Status == StatusFail && e.Failure != "" {
		parts = append(parts, s.fail.Render(e.Failure))
	}
	if d := e.Duration(); d >= 500*time.Millisecond {
		parts = append(parts, s.meta.Render(fmt.Sprintf("(%.1fs)", d.Seconds())))
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
	s := curEntryStyles()
	head := s.ok.Render("✓") + " " + collapsedLabel(e.Name, len(e.Children))
	lines := []string{head}
	for _, c := range e.Children {
		label := c.Name
		if a := briefArgs(c.Args); a != "" {
			label = a
		}
		lines = append(lines, "  "+s.body.Render(runewidth.Truncate(label, width-2, "…")))
	}
	return lines
}

func entrySkill(e Item) []string {
	s := curEntryStyles()
	label := "◆ skill " + e.Name
	if e.Version != "" {
		label += " v" + e.Version
	}
	return []string{s.skill.Render(label)}
}

func entryReflection(e Item, width int) []string {
	s := curEntryStyles()
	lines := []string{s.reflection.Render("↻ reflection")}
	for _, ln := range wrapProse(e.Text, width-2) {
		lines = append(lines, "  "+s.reflection.Render(ln))
	}
	return lines
}

func entryAssistant(e Item, width int) []string {
	s := curEntryStyles()
	lines := []string{s.assistant.Render("⏺ assistant")}
	for _, ln := range wrapProse(e.Text, width) {
		lines = append(lines, s.assistant.Render(ln))
	}
	return lines
}

func entryUser(e Item, width int) []string {
	s := curEntryStyles()
	var lines []string
	for i, ln := range wrapProse(strings.TrimRight(e.Text, "\n"), width-2) {
		prefix := "  "
		if i == 0 {
			prefix = "› "
		}
		lines = append(lines, s.user.Render(prefix+ln))
	}
	return lines
}

func entryThinking(e Item, width int) []string {
	s := curEntryStyles()
	out := []string{}
	for _, ln := range wrapProse(e.Text, width) {
		out = append(out, s.thinking.Render(ln))
	}
	return out
}

func entrySystem(e Item, width int) []string {
	s := curEntryStyles()
	var out []string
	for _, ln := range strings.Split(strings.TrimRight(e.Text, "\n"), "\n") {
		out = append(out, s.meta.Render(runewidth.Truncate(ln, width, "…")))
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
	s := curEntryStyles()
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = "  " + s.body.Render(runewidth.Truncate(ln, width-2, "…"))
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

// toolAction renders a tool call as a concise "what it actually did" — the real
// path/command, not raw JSON.
func toolAction(it Item) string {
	s := curEntryStyles()
	if it.Kind == ItemSkill {
		label := "◆ skill " + it.Name
		if it.Version != "" {
			label += " v" + it.Version
		}
		return s.skill.Render(label)
	}
	switch it.Name {
	case "read_file":
		return "Read(" + readFileDesc(it.Args) + ")"
	case "list_files":
		return "List(" + firstArg(it.Args) + ")"
	case "grep":
		return "Grep(" + firstArg(it.Args) + ")"
	case "run_command":
		return "$ " + firstArg(it.Args)
	case "edit_file":
		return "Update(" + firstArg(it.Args) + ")"
	case "apply_patch":
		return "Apply Patch(" + firstArg(it.Args) + ")"
	case "create_file":
		return "Create(" + firstArg(it.Args) + ")"
	default:
		if a := briefArgs(it.Args); a != "" {
			return it.Name + " " + a
		}
		return it.Name
	}
}

// readFileDesc builds a description of a read_file call with its path and any
// offset/limit so consecutive reads of the same file are distinguishable.
func readFileDesc(args string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return firstArg(args)
	}
	path, _ := m["path"].(string)
	offset, hasOff := numArg(m, "offset")
	limit, hasLim := numArg(m, "limit")
	var parts []string
	if path != "" {
		parts = append(parts, path)
	}
	if hasOff && offset > 1 {
		parts = append(parts, fmt.Sprintf("L%d", offset))
	}
	if hasLim {
		parts = append(parts, fmt.Sprintf("+%d", limit))
	}
	if len(parts) == 0 {
		return firstArg(args)
	}
	return strings.Join(parts, ", ")
}

// numArg extracts a numeric argument from the tool input. float64 is what
// encoding/json produces for JSON numbers.
func numArg(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case string:
		var i int
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i, true
		}
	}
	return 0, false
}

// firstArg pulls the primary argument out of a tool's JSON args (the path /
// command / pattern), falling back to a flattened brief.
func firstArg(args string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return briefArgs(args)
	}
	for _, k := range []string{"path", "command", "pattern", "query", "name", "dir"} {
		if v, ok := m[k]; ok {
			return fmt.Sprint(v)
		}
	}
	for _, v := range m { // any value, deterministic enough for a single-key tool
		return fmt.Sprint(v)
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func humanDuration(d time.Duration) string {
	if s := d.Seconds(); s >= 1 {
		return fmt.Sprintf("%.0fs", s)
	}
	return "<1s"
}

// summarizeTools counts a group's tools by kind into a human phrase, e.g.
// "read 3 files, ran 1 command". Counting is not merging — each tool still gets
// its own detail line below the header.
func summarizeTools(tools []Item) string {
	var order []string
	counts := map[string]int{}
	for _, it := range tools {
		key := it.Name
		if it.Kind == ItemSkill {
			key = "__skill"
		}
		if counts[key] == 0 {
			order = append(order, key)
		}
		counts[key]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, stepVerb(k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

func stepVerb(tool string, n int) string {
	switch tool {
	case "read_file":
		return fmt.Sprintf("read %d %s", n, plural(n, "file", "files"))
	case "list_files":
		return fmt.Sprintf("listed %d %s", n, plural(n, "directory", "directories"))
	case "grep":
		return fmt.Sprintf("searched %d %s", n, plural(n, "time", "times"))
	case "run_command":
		return fmt.Sprintf("ran %d %s", n, plural(n, "command", "commands"))
	case "edit_file":
		return fmt.Sprintf("edited %d %s", n, plural(n, "file", "files"))
	case "create_file":
		return fmt.Sprintf("created %d %s", n, plural(n, "file", "files"))
	case "apply_patch":
		return fmt.Sprintf("applied %d %s", n, plural(n, "patch", "patches"))
	case "__skill":
		return fmt.Sprintf("loaded %d %s", n, plural(n, "skill", "skills"))
	default:
		return fmt.Sprintf("%s ×%d", tool, n)
	}
}

// toolDetailLines renders one tool as its actual action (the real command). A
// failure always prints its body (the signal); a mutation tool prints its body
// even on success — the user needs to see what changed.
func toolDetailLines(it Item, width int) []string {
	s := curEntryStyles()
	mark := s.ok.Render("✓")
	if it.Status == StatusFail {
		mark = s.fail.Render("✗")
	}
	line := "   " + mark + " " + toolAction(it)
	if d := it.Duration(); d >= 500*time.Millisecond {
		line += "  " + s.meta.Render(fmt.Sprintf("(%.1fs)", d.Seconds()))
	}
	lines := []string{line}
	show := it.Status == StatusFail || mutationTools[it.Name]
	if show && strings.TrimSpace(it.Text) != "" {
		limit := failBodyLines
		if it.Status != StatusFail {
			limit = 20 // a successful edit may have more useful output (diff, new content)
		}
		for _, b := range indentBody(it.Text, width-3, limit) {
			lines = append(lines, "   "+b)
		}
	}
	return lines
}
