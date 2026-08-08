package dialog

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"code-agent/cmd/codeagent/tui/layout"
	"code-agent/cmd/codeagent/tui/styles"
	"code-agent/cmd/codeagent/tui/theme"
	"code-agent/cmd/codeagent/tui/util"
	"code-agent/internal/agent"
)

// PermissionAction is the user's decision on a side-effecting tool call.
type PermissionAction string

// Permission responses
const (
	PermissionAllow           PermissionAction = "allow"
	PermissionAllowForSession PermissionAction = "allow_session"
	PermissionDeny            PermissionAction = "deny"
)

// ApprovalRequest is the dialog's view of a pending side-effecting tool call:
// the tool name, its raw JSON input, and the channel the runner goroutine
// blocks on until the user answers.
type ApprovalRequest struct {
	Tool  string
	Input json.RawMessage
	Reply chan agent.Verdict
}

// PermissionResponseMsg tells the app that the dialog answered so the app can
// close it. The verdict itself has already been delivered on Request.Reply by
// the dialog before this message is emitted.
type PermissionResponseMsg struct {
	Request ApprovalRequest
	Action  PermissionAction
}

// PermissionGranter persists an "always allow" rule for a tool. It mirrors the
// root package's PermissionGranter shape (AllowAlways) so the dialog needs no
// root import; nil disables the allow-for-session option.
type PermissionGranter interface {
	AllowAlways(toolName string) (rule string, err error)
}

// PermissionDialogCmp is the permission request dialog.
type PermissionDialogCmp interface {
	tea.Model
	layout.Bindings
	SetPermissions(req ApprovalRequest) tea.Cmd
}

type permissionKeyMap struct {
	Left         key.Binding
	Right        key.Binding
	Tab          key.Binding
	EnterSpace   key.Binding
	Allow        key.Binding
	AllowSession key.Binding
	Deny         key.Binding
	Preview      key.Binding
	Escape       key.Binding
}

var permissionKeys = permissionKeyMap{
	Left: key.NewBinding(
		key.WithKeys("left"),
		key.WithHelp("←", "switch options"),
	),
	Right: key.NewBinding(
		key.WithKeys("right"),
		key.WithHelp("→", "switch options"),
	),
	Tab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch options"),
	),
	EnterSpace: key.NewBinding(
		key.WithKeys("enter", " "),
		key.WithHelp("enter/space", "confirm"),
	),
	Allow: key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "allow"),
	),
	AllowSession: key.NewBinding(
		key.WithKeys("s"),
		key.WithHelp("s", "allow for session"),
	),
	Deny: key.NewBinding(
		key.WithKeys("d"),
		key.WithHelp("d", "deny"),
	),
	Preview: key.NewBinding(
		key.WithKeys("v", "V"),
		key.WithHelp("v", "toggle preview"),
	),
	Escape: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "deny"),
	),
}

// permissionDialogCmp is the approval dialog. The dialog answers the request
// itself — it writes the verdict to ApprovalRequest.Reply (unblocking the
// runner goroutine) and applies the "always allow" grant via the injected
// granter — then emits PermissionResponseMsg so the app can close it. The app
// must not resend the verdict or re-apply the grant.
type permissionDialogCmp struct {
	width           int
	height          int
	req             ApprovalRequest
	windowSize      tea.WindowSizeMsg
	contentViewport viewport.Model
	selectedOption  int // 0: Allow, 1: Allow for session, 2: Deny
	showPreview     bool
	granter         PermissionGranter
}

func (p *permissionDialogCmp) Init() tea.Cmd {
	return p.contentViewport.Init()
}

// optionCount is 3 with a granter (allow / allow-for-session / deny), else 2.
func (p *permissionDialogCmp) optionCount() int {
	if p.granter != nil {
		return 3
	}
	return 2
}

func (p *permissionDialogCmp) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.windowSize = msg
		return p, p.SetSize()
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, permissionKeys.Right) || key.Matches(msg, permissionKeys.Tab):
			p.selectedOption = (p.selectedOption + 1) % p.optionCount()
			return p, nil
		case key.Matches(msg, permissionKeys.Left):
			p.selectedOption = (p.selectedOption + p.optionCount() - 1) % p.optionCount()
			return p, nil
		case key.Matches(msg, permissionKeys.EnterSpace):
			return p, p.selectCurrentOption()
		case key.Matches(msg, permissionKeys.Allow):
			return p, p.respond(PermissionAllow)
		case key.Matches(msg, permissionKeys.AllowSession):
			if p.granter != nil {
				return p, p.respond(PermissionAllowForSession)
			}
			return p, nil
		case key.Matches(msg, permissionKeys.Deny):
			return p, p.respond(PermissionDeny)
		case key.Matches(msg, permissionKeys.Preview):
			p.showPreview = !p.showPreview
			return p, nil
		case key.Matches(msg, permissionKeys.Escape):
			return p, p.respond(PermissionDeny)
		case msg.String() == "ctrl+c":
			return p, tea.Batch(p.respond(PermissionDeny), tea.Quit)
		default:
			vp, cmd := p.contentViewport.Update(msg)
			p.contentViewport = vp
			return p, cmd
		}
	}
	return p, nil
}

// respond answers the request: allow/deny map to the agent verdict, and
// allow-for-session first persists an "always allow" rule (best-effort — a
// failed persist still allows this call), matching the old approval overlay.
func (p *permissionDialogCmp) respond(action PermissionAction) tea.Cmd {
	if action == PermissionAllowForSession && p.granter != nil {
		_, _ = p.granter.AllowAlways(p.req.Tool)
	}
	verdict := agent.VerdictDeny
	if action != PermissionDeny {
		verdict = agent.VerdictAllow
	}
	if p.req.Reply != nil {
		p.req.Reply <- verdict
	}
	return util.CmdHandler(PermissionResponseMsg{Request: p.req, Action: action})
}

func (p *permissionDialogCmp) selectCurrentOption() tea.Cmd {
	switch p.selectedOption {
	case 0:
		return p.respond(PermissionAllow)
	case 1:
		if p.granter != nil {
			return p.respond(PermissionAllowForSession)
		}
		return p.respond(PermissionDeny)
	default:
		return p.respond(PermissionDeny)
	}
}

func (p *permissionDialogCmp) styleViewport() string {
	t := theme.CurrentTheme()
	return lipgloss.NewStyle().Background(t.Background()).Render(p.contentViewport.View())
}

func (p *permissionDialogCmp) render() string {
	t := theme.CurrentTheme()
	base := styles.BaseStyle()

	title := base.
		Bold(true).
		Width(p.width - 4).
		Foreground(t.Primary()).
		Render("Permission Required")

	innerW := p.width - 4
	if innerW < 20 {
		innerW = 20
	}
	lines := renderApprovalCard(p.req, p.selectedOption, p.optionCount(), innerW)
	if p.showPreview {
		lines = append(lines, renderApprovalPreview(p.req, innerW)...)
	}

	p.contentViewport.Width = innerW
	p.contentViewport.Height = max(1, p.height-lipgloss.Height(title)-5)
	p.contentViewport.SetContent(strings.Join(lines, "\n"))

	content := lipgloss.JoinVertical(
		lipgloss.Top,
		title,
		base.Render(strings.Repeat(" ", innerW)),
		p.styleViewport(),
	)

	return base.
		Padding(1, 0, 0, 1).
		Border(lipgloss.RoundedBorder()).
		BorderBackground(t.Background()).
		BorderForeground(t.TextMuted()).
		Width(p.width).
		Height(p.height).
		Render(content)
}

func (p *permissionDialogCmp) View() string {
	return p.render()
}

func (p *permissionDialogCmp) BindingKeys() []key.Binding {
	return layout.KeyMapToSlice(permissionKeys)
}

func (p *permissionDialogCmp) SetSize() tea.Cmd {
	if p.windowSize.Width < 20 {
		p.width = 40
		p.height = 10
		return nil
	}
	switch p.req.Tool {
	case "run_command":
		p.width = int(float64(p.windowSize.Width) * 0.4)
		p.height = int(float64(p.windowSize.Height) * 0.3)
	case "edit_file", "create_file", "apply_patch":
		p.width = int(float64(p.windowSize.Width) * 0.8)
		p.height = int(float64(p.windowSize.Height) * 0.8)
	default:
		p.width = int(float64(p.windowSize.Width) * 0.7)
		p.height = int(float64(p.windowSize.Height) * 0.5)
	}
	return nil
}

func renderEditPreview(raw map[string]any, width int) []string {
	t := theme.CurrentTheme()
	bg := t.Background()
	meta := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted())
	fail := lipgloss.NewStyle().Background(bg).Foreground(t.Error())
	ok := lipgloss.NewStyle().Background(bg).Foreground(t.Success())

	old, _ := raw["old"].(string)
	new, _ := raw["new"].(string)
	if old == "" && new == "" {
		return nil
	}
	lines := []string{meta.Render("── diff preview ──")}
	if old != "" && new != "" {
		for _, ln := range strings.Split(old, "\n") {
			lines = append(lines, fail.Render(runewidth.Truncate("- "+ln, width, "…")))
		}
		for _, ln := range strings.Split(new, "\n") {
			lines = append(lines, ok.Render(runewidth.Truncate("+ "+ln, width, "…")))
		}
	} else if old != "" {
		lines = append(lines, fail.Render("removing:"))
		for _, ln := range strings.Split(old, "\n") {
			lines = append(lines, fail.Render(runewidth.Truncate("- "+ln, width, "…")))
		}
	}
	return lines
}

func renderPatchPreview(raw map[string]any, width int) []string {
	t := theme.CurrentTheme()
	bg := t.Background()
	meta := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted())
	fail := lipgloss.NewStyle().Background(bg).Foreground(t.Error())
	ok := lipgloss.NewStyle().Background(bg).Foreground(t.Success())
	body := lipgloss.NewStyle().Background(bg).Foreground(t.Text())

	patch, _ := raw["patch"].(string)
	if patch == "" {
		return nil
	}
	lines := []string{meta.Render("── patch preview ──")}
	for _, ln := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		trunc := runewidth.Truncate(ln, width, "…")
		if strings.HasPrefix(ln, "+") {
			lines = append(lines, ok.Render(trunc))
		} else if strings.HasPrefix(ln, "-") {
			lines = append(lines, fail.Render(trunc))
		} else {
			lines = append(lines, body.Render(trunc))
		}
	}
	return lines
}

func renderCreatePreview(raw map[string]any, width int) []string {
	t := theme.CurrentTheme()
	bg := t.Background()
	meta := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted())
	body := lipgloss.NewStyle().Background(bg).Foreground(t.Text())

	content, _ := raw["content"].(string)
	if content == "" {
		return nil
	}
	lines := []string{meta.Render("── file content ──")}
	preview := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(preview) > 20 {
		preview = append(preview[:20:20], meta.Render(fmt.Sprintf("… %d more lines", len(preview)-20)))
	}
	for _, ln := range preview {
		lines = append(lines, body.Render(runewidth.Truncate(ln, width, "…")))
	}
	return lines
}

func renderCommandPreview(raw map[string]any, width int) []string {
	t := theme.CurrentTheme()
	bg := t.Background()
	meta := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted())
	ok := lipgloss.NewStyle().Background(bg).Foreground(t.Success())

	cmd, _ := raw["command"].(string)
	if cmd == "" {
		return nil
	}
	return []string{
		meta.Render("── command ──"),
		ok.Render(runewidth.Truncate("$ "+cmd, width, "…")),
	}
}

func renderJSONPreview(raw map[string]any, width int) []string {
	t := theme.CurrentTheme()
	bg := t.Background()
	meta := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted())
	body := lipgloss.NewStyle().Background(bg).Foreground(t.Text())

	lines := []string{meta.Render("── tool input ──")}
	for k, v := range raw {
		lines = append(lines, body.Render(runewidth.Truncate(fmt.Sprintf("  %s: %v", k, v), width, "…")))
	}
	return lines
}

// approvalSelector shows a/s/d with the active choice highlighted — the
// ←/→/tab-movable cursor the selectedOption field drives.
func approvalSelector(idx, optionCount int) string {
	labels := []string{"[a] allow", "[s] allow for session", "[d] deny"}
	labels = labels[:optionCount]

	t := theme.CurrentTheme()
	bg := t.Background()
	parts := make([]string, len(labels))
	for i, label := range labels {
		if i == idx {
			parts[i] = lipgloss.NewStyle().Background(bg).Foreground(t.Primary()).Bold(true).Render("▶ " + label)
		} else {
			parts[i] = lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted()).Render("  " + label)
		}
	}
	help := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted()).
		Render("[v] preview  (←/→ select · enter confirm · esc deny)")
	return strings.Join(parts, "  ") + "  " + help
}


func (p *permissionDialogCmp) SetPermissions(req ApprovalRequest) tea.Cmd {
	p.req = req
	p.selectedOption = 0
	p.showPreview = false
	return p.SetSize()
}

// NewPermissionDialogCmp creates the approval dialog. granter may be nil — the
// allow-for-session option is then omitted.
func NewPermissionDialogCmp(granter PermissionGranter) PermissionDialogCmp {
	return &permissionDialogCmp{
		contentViewport: viewport.New(0, 0),
		granter:         granter,
	}
}

const maxPreviewLines = 8 // max rows shown per argument in the preview

// renderApprovalCard renders the approval card body in the live region: the
// tool name + a readable argument preview, plus the a/s/d selector the user
// navigates with ←/→/tab and confirms with enter/space. The dialog's own
// border wraps it; no inner box is drawn.
func renderApprovalCard(req ApprovalRequest, approveIdx, optionCount, width int) []string {
	innerW := width - 4
	if innerW < 20 {
		innerW = 20
	}
	preview := approvalPreview(req.Tool, string(req.Input), innerW)
	sel := approvalSelector(approveIdx, optionCount)
	return append(preview, "", sel)
}

// approvalPreview renders the tool input as readable key-value lines so the
// user can see what they're approving — the REPL's per-field display, now in
// the TUI.
func approvalPreview(tool string, input string, width int) []string {
	t := theme.CurrentTheme()
	bg := t.Background()
	fail := lipgloss.NewStyle().Background(bg).Foreground(t.Error())
	meta := lipgloss.NewStyle().Background(bg).Foreground(t.TextMuted())
	body := lipgloss.NewStyle().Background(bg).Foreground(t.Text())

	var raw map[string]any
	json.Unmarshal([]byte(input), &raw)

	var lines []string
	// The tool name gets a bold header line.
	lines = append(lines, fail.Render("▸ run "+tool+"?"))

	// Ordered fields: show the primary ones first, then the rest.
	for _, key := range []string{"command", "path", "old", "new", "patch", "message"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		s := fmt.Sprint(v)
		if s == "" {
			lines = append(lines, meta.Render(fmt.Sprintf("  %s: (empty)", key)))
		} else {
			lines = append(lines, meta.Render(fmt.Sprintf("  %s:", key)))
			for _, ln := range previewLines(s, width-4, maxPreviewLines) {
				lines = append(lines, body.Render("    "+ln))
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
		lines = append(lines, meta.Render(fmt.Sprintf("  %s:", k)))
		for _, ln := range previewLines(s, width-4, 4) {
			lines = append(lines, body.Render("    "+ln))
		}
	}
	return lines
}

func previewLines(s string, width, max int) []string {
	t := theme.CurrentTheme()
	meta := lipgloss.NewStyle().Background(t.Background()).Foreground(t.TextMuted())
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > max {
		extra := len(lines) - max
		lines = append(lines[:max:max], meta.Render(fmt.Sprintf("  … %d more lines", extra)))
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = runewidth.Truncate(ln, width, "…")
	}
	return out
}

// renderApprovalPreview renders a diff-like preview of what the tool will do
// when approved — toggled by 'v' in the approval dialog.
func renderApprovalPreview(req ApprovalRequest, width int) []string {
	innerW := width - 4
	if innerW < 20 {
		innerW = 20
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(req.Input), &raw); err != nil {
		// JSON parse failure — show raw input as a string instead of crashing
		return previewLines(string(req.Input), innerW, 20)
	}

	switch req.Tool {
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


