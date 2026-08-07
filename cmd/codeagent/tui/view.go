package tui

import (
	"code-agent/cmd/codeagent/tui/theme"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"code-agent/internal/agent"

	"github.com/charmbracelet/lipgloss"
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
	return strings.Split(theme.Default.ApproveBox().Width(innerW).Render(strings.Join(lines, "\n")), "\n")
}

// approvalPreview renders the tool input as readable key-value lines so the user
// can see what they're approving — the REPl's per-field display, now in the TUI.
func approvalPreview(tool string, input string, width int) []string {
	var raw map[string]any
	json.Unmarshal([]byte(input), &raw)

	var lines []string
	// The tool name gets a bold header line.
	lines = append(lines, theme.Default.Fail.Render("▸ run "+tool+"?"))

	// Ordered fields: show the primary ones first, then the rest.
	for _, key := range []string{"command", "path", "old", "new", "patch", "message"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		s := fmt.Sprint(v)
		if s == "" {
			lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  %s: (empty)", key)))
		} else {
			lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  %s:", key)))
			for _, ln := range previewLines(s, width-4, maxPreviewLines) {
				lines = append(lines, theme.Default.Body.Render("    "+ln))
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
		lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  %s:", k)))
		for _, ln := range previewLines(s, width-4, 4) {
			lines = append(lines, theme.Default.Body.Render("    "+ln))
		}
	}
	return lines
}

func previewLines(s string, width, max int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > max {
		extra := len(lines) - max
		lines = append(lines[:max:max], theme.Default.Meta.Render(fmt.Sprintf("  … %d more lines", extra)))
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = runewidth.Truncate(ln, width, "…")
	}
	return out
}

// renderApprovalPreview renders a diff-like preview of what the tool will do
// when approved — toggled by 'v' in the approval card.
func renderApprovalPreview(req approvalReq, width int) []string {
	innerW := width - 4
	if innerW < 20 {
		innerW = 20
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(req.input), &raw); err != nil {
		// JSON parse failure — show raw input as a string instead of crashing
		return previewLines(string(req.input), innerW, 20)
	}

	switch req.tool {
	case "edit_file":
		return renderEditPreview(raw, innerW)
	case "apply_patch":
		return renderPatchPreview(raw, innerW)
	case "create_file":
		return renderCreatePreview(raw, innerW)
	case "run_command":
		return renderCommandPreview(raw, innerW)
	default:
		return renderJSONPreview(raw, innerW)
	}
}

func renderEditPreview(raw map[string]any, width int) []string {
	old, _ := raw["old"].(string)
	new, _ := raw["new"].(string)
	if old == "" && new == "" {
		return nil
	}
	lines := []string{theme.Default.Meta.Render("── diff preview ──")}
	if old != "" && new != "" {
		for _, ln := range strings.Split(old, "\n") {
			lines = append(lines, theme.Default.Fail.Render(runewidth.Truncate("- "+ln, width, "…")))
		}
		for _, ln := range strings.Split(new, "\n") {
			lines = append(lines, theme.Default.OK.Render(runewidth.Truncate("+ "+ln, width, "…")))
		}
	} else if old != "" {
		lines = append(lines, theme.Default.Fail.Render("removing:"))
		for _, ln := range strings.Split(old, "\n") {
			lines = append(lines, theme.Default.Fail.Render(runewidth.Truncate("- "+ln, width, "…")))
		}
	}
	return lines
}

func renderPatchPreview(raw map[string]any, width int) []string {
	patch, _ := raw["patch"].(string)
	if patch == "" {
		return nil
	}
	lines := []string{theme.Default.Meta.Render("── patch preview ──")}
	for _, ln := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		trunc := runewidth.Truncate(ln, width, "…")
		if strings.HasPrefix(ln, "+") {
			lines = append(lines, theme.Default.OK.Render(trunc))
		} else if strings.HasPrefix(ln, "-") {
			lines = append(lines, theme.Default.Fail.Render(trunc))
		} else {
			lines = append(lines, theme.Default.Body.Render(trunc))
		}
	}
	return lines
}

func renderCreatePreview(raw map[string]any, width int) []string {
	content, _ := raw["content"].(string)
	if content == "" {
		return nil
	}
	lines := []string{theme.Default.Meta.Render("── file content ──")}
	preview := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(preview) > 20 {
		preview = append(preview[:20:20], theme.Default.Meta.Render(fmt.Sprintf("… %d more lines", len(preview)-20)))
	}
	for _, ln := range preview {
		lines = append(lines, theme.Default.Body.Render(runewidth.Truncate(ln, width, "…")))
	}
	return lines
}

func renderCommandPreview(raw map[string]any, width int) []string {
	cmd, _ := raw["command"].(string)
	if cmd == "" {
		return nil
	}
	return []string{
		theme.Default.Meta.Render("── command ──"),
		theme.Default.OK.Render(runewidth.Truncate("$ "+cmd, width, "…")),
	}
}

func renderJSONPreview(raw map[string]any, width int) []string {
	lines := []string{theme.Default.Meta.Render("── tool input ──")}
	for k, v := range raw {
		lines = append(lines, theme.Default.Body.Render(runewidth.Truncate(fmt.Sprintf("  %s: %v", k, v), width, "…")))
	}
	return lines
}

// approvalSelector shows y/n with the active choice highlighted — the ↑/↓
// movable cursor the approveIdx field drives.
func approvalSelector(idx int) string {
	labels := []string{"[y] allow once", "[a] always allow", "[n] deny"}
	parts := make([]string, len(labels))
	for i, label := range labels {
		if i == idx {
			parts[i] = theme.Default.ApproveHl.Render("▶ " + label)
		} else {
			parts[i] = theme.Default.ApproveDim.Render("  " + label)
		}
	}
	return strings.Join(parts, "  ") + "  " + theme.Default.Meta.Render("[v] preview  (↑/↓ select · enter confirm · esc cancel)")
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

// renderPlanApprovalCard renders a plan for user approval in the live region.
func renderPlanApprovalCard(plan agent.Plan, width int) []string {
	innerW := width - 4
	if innerW < 20 {
		innerW = 20
	}

	var lines []string
	lines = append(lines, theme.Default.Skill.Render("▸ Plan: "+plan.Title))
	lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  ID: %s  |  Saved: %s", plan.ID, plan.FilePath)))
	lines = append(lines, "")

	// Preview first ~15 lines of the plan content.
	contentLines := strings.Split(plan.Content, "\n")
	maxLines := 15
	if len(contentLines) < maxLines {
		maxLines = len(contentLines)
	}
	for i := 0; i < maxLines; i++ {
		ln := runewidth.Truncate(contentLines[i], innerW-2, "…")
		lines = append(lines, theme.Default.Body.Render("  "+ln))
	}
	if len(contentLines) > maxLines {
		lines = append(lines, theme.Default.Meta.Render(fmt.Sprintf("  … %d more lines", len(contentLines)-maxLines)))
	}
	lines = append(lines, "")
	lines = append(lines, theme.Default.Meta.Render("  [a] approve  [r] reject  (esc/r to reject)"))

	return strings.Split(theme.Default.ApproveBox().Width(innerW).Render(strings.Join(lines, "\n")), "\n")
}
