package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// stepBuf accumulates one step — a model call (its think time) plus the tools it
// then ran — so the step can be printed as a single "Thought for Ns, read 1 file"
// header with the real commands beneath it. It does NOT merge events (each tool
// keeps its own detail line); it only groups them for the header.
type stepBuf struct {
	active   bool
	elapsed  time.Duration // model-call duration → "Thought for Ns"
	thinking string        // the model's reasoning text, shown when the step is expanded
	// thinkingFinal distinguishes a persisted EventThinking snapshot from an
	// ephemeral reasoning_delta preview. A preview without a final snapshot is
	// discarded at model_finished (for example after a resilient fallback).
	thinkingFinal bool
	tools         []Item // the tools (and skills) run in this step, in order
}

// renderStep formats a completed step: a "Thought for Ns, <summary>" header and
// one indented detail line per tool showing the actual command — so the agent's
// actions are visible, not a black box. When expanded, the reasoning text is
// shown beneath the header (the live region's per-step expand; in committed
// scrollback steps are always collapsed).
func renderStep(s stepBuf, width int, expanded bool) []string {
	if !s.active || (s.elapsed == 0 && len(s.tools) == 0) {
		return nil
	}
	caret := "  "
	if s.thinking != "" {
		caret = "▸ "
		if expanded {
			caret = "▾ "
		}
	}
	header := caret + "Thought for " + humanDuration(s.elapsed)
	if sum := summarizeTools(s.tools); sum != "" {
		header += ", " + sum
	}
	lines := []string{styleThinking.Render(header)}
	if expanded && s.thinking != "" {
		for _, ln := range wrapProse(s.thinking, width-4) {
			lines = append(lines, "    "+styleBody.Render(ln))
		}
	}
	for _, it := range s.tools {
		lines = append(lines, toolDetailLines(it, width)...)
	}
	return lines
}

// fmtStepHeader returns the "Thought for Ns, read 1 file" header for the current
// step — used by View() to show thinking in the live region. Pure; no I/O.
func fmtStepHeader(s stepBuf) string {
	h := "Thought for " + humanDuration(s.elapsed)
	if sum := summarizeTools(s.tools); sum != "" {
		h += ", " + sum
	}
	return h
}

func humanDuration(d time.Duration) string {
	if s := d.Seconds(); s >= 1 {
		return fmt.Sprintf("%.0fs", s)
	}
	return "<1s"
}

// summarizeTools counts the step's tools by kind into a human phrase, e.g.
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

// mutationTools are the tools whose output the user always wants to see, even on
// success — what was edited/created/applied/committed is the primary signal.
var mutationTools = map[string]bool{
	"edit_file":   true,
	"create_file": true,
	"apply_patch": true,
	"git_commit":  true,
}

// toolDetailLines renders one tool as its actual action (the real command). A
// failure always prints its body (the signal); a mutation tool prints its body
// even on success — the user needs to see what changed.
func toolDetailLines(it Item, width int) []string {
	mark := styleOK.Render("✓")
	if it.Status == StatusFail {
		mark = styleFail.Render("✗")
	}
	line := "   " + mark + " " + toolAction(it)
	if d := it.Duration(); d >= 500*time.Millisecond {
		line += "  " + styleMeta.Render(fmt.Sprintf("(%.1fs)", d.Seconds()))
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

// toolAction renders a tool call as a concise "what it actually did" — the real
// path/command, not raw JSON.
func toolAction(it Item) string {
	if it.Kind == ItemSkill {
		s := "◆ skill " + it.Name
		if it.Version != "" {
			s += " v" + it.Version
		}
		return styleSkill.Render(s)
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
