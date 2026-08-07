package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// approvalOverlay is the side-effecting-tool card: "run create_file?" with a
// readable argument preview and a y/a/n selector. The runner goroutine is
// blocked on req.reply until the user answers — the same pause the REPL gets
// from a readline y/N, as a card in the live region now. granter persists an
// "always allow" rule (for an MCP tool, the whole server) so future matching
// calls skip the prompt; nil means the card offers only allow-once / deny.
type approvalOverlay struct {
	req         approvalReq
	idx         int    // 0 = allow once, 1 = always allow, 2 = deny — ↑/↓ switches
	showPreview bool   // 'v' toggles the diff preview below the approval card
	granter     PermissionGranter
}

// Key drives the approval card: ↑/↓ moves between allow-once, always-allow,
// and deny; Enter confirms the selection; Esc denies. Direct keys still work:
// y/o = allow once, a = always allow, n = deny. "Always allow" persists a rule
// via the granter so future matching calls skip the prompt; with no granter
// wired it falls back to once. The card is modal — it consumes every key, since
// a tool call is waiting. ctrl+c denies and quits.
func (o *approvalOverlay) Key(msg tea.KeyMsg, _ *model) (Overlay, bool, tea.Cmd) {
	answer := func(approved, always bool) (Overlay, bool, tea.Cmd) {
		if approved && always && o.granter != nil {
			// Best-effort: a failed persist still allows this call.
			_, _ = o.granter.AllowAlways(o.req.tool)
		}
		if approved {
			o.req.reply <- agent.VerdictAllow
		} else {
			o.req.reply <- agent.VerdictDeny
		}
		return nil, true, nil // listeners stay alive (approvalMsg already re-issued waitForApproval)
	}
	switch msg.String() {
	case "up", "k", "ctrl+p":
		if o.idx > 0 {
			o.idx--
		}
	case "down", "j", "ctrl+n":
		if o.idx < 2 {
			o.idx++
		}
	case "v", "V":
		o.showPreview = !o.showPreview
	case "enter":
		return answer(o.idx != 2, o.idx == 1)
	case "y", "Y", "o", "O":
		return answer(true, false)
	case "a", "A":
		return answer(true, true)
	case "n", "N", "esc":
		return answer(false, false)
	case "ctrl+c":
		o.req.reply <- agent.VerdictDeny
		return nil, true, tea.Quit
	}
	return o, true, nil
}

func (o *approvalOverlay) View(width int, _ *model) []string {
	lines := renderApprovalCard(o.req, o.idx, width)
	if o.showPreview {
		lines = append(lines, renderApprovalPreview(o.req, width)...)
	}
	return lines
}

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
