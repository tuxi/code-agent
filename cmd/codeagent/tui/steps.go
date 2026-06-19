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
	active  bool
	elapsed time.Duration // model-call duration → "Thought for Ns"
	tools   []Item        // the tools (and skills) run in this step, in order
}

// renderStep formats a completed step: a "Thought for Ns, <summary>" header and
// one indented detail line per tool showing the actual command — so the agent's
// actions are visible, not a black box.
func renderStep(s stepBuf, width int) []string {
	if !s.active || (s.elapsed == 0 && len(s.tools) == 0) {
		return nil
	}
	header := "Thought for " + humanDuration(s.elapsed)
	if sum := summarizeTools(s.tools); sum != "" {
		header += ", " + sum
	}
	lines := []string{styleThinking.Render(header)}
	for _, it := range s.tools {
		lines = append(lines, toolDetailLines(it, width)...)
	}
	return lines
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

// toolDetailLines renders one tool as its actual action (the real command), with
// a failure printing its body (the signal).
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
	if it.Status == StatusFail && strings.TrimSpace(it.Text) != "" {
		for _, b := range indentBody(it.Text, width-3, failBodyLines) {
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
	arg := firstArg(it.Args)
	switch it.Name {
	case "read_file":
		return "Read(" + arg + ")"
	case "list_files":
		return "List(" + arg + ")"
	case "grep":
		return "Grep(" + arg + ")"
	case "run_command":
		return "$ " + arg
	case "edit_file", "create_file", "apply_patch":
		return it.Name + "(" + arg + ")"
	default:
		if a := briefArgs(it.Args); a != "" {
			return it.Name + " " + a
		}
		return it.Name
	}
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
