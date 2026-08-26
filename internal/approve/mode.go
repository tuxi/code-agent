package approve

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"

	"code-agent/internal/agent"
	"code-agent/internal/sandbox"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
	"code-agent/internal/workspace"
)

// Mode is the workspace approval tier. It is a separate knob from the
// permissions.allow/deny rules (which stay owned by the approval card's
// "always allow" grants): the rules say "these tools never prompt", the tier
// says "what happens when no rule matched". Tiers mirror Codex's permission
// presets:
//
//	ask   — every un-matched side-effecting call prompts (the default, today's
//	        behavior). Matched allow rules still auto-run; deny still wins.
//	auto  — in-workspace work auto-runs: file writes (edit/create/apply_patch),
//	        git_commit, non-network run_command, and MCP server tools. Network
//	        commands and out-of-workspace paths still prompt.
//	full  — every side-effecting call auto-runs, including network commands
//	        and out-of-workspace paths. Hard lines stay: deny rules,
//	        protected paths, and blocked commands are never auto-crossed.
//
// ask_user is deliberately NOT gated by the tier: it is the model choosing to
// consult the user about a genuinely ambiguous task (not a permission gate),
// so it must reach the user in every mode.
type Mode string

const (
	ModeAsk  Mode = "ask"
	ModeAuto Mode = "auto"
	ModeFull Mode = "full"
)

// ValidMode reports whether m is one of the three tiers.
func ValidMode(m Mode) bool {
	switch m {
	case ModeAsk, ModeAuto, ModeFull:
		return true
	default:
		return false
	}
}

// ParseMode maps a wire string to a Mode. "full_access" is accepted as a
// legacy alias of "full" (the automation permission_mode vocabulary predates
// the tier rename). ok=false for anything else — callers treat that as
// "not set" and fall back to the workspace default.
func ParseMode(s string) (Mode, bool) {
	switch Mode(s) {
	case ModeAsk, ModeAuto, ModeFull:
		return Mode(s), true
	case "full_access":
		return ModeFull, true
	default:
		return ModeAsk, false
	}
}

// ModeFromSettings returns the effective tier from the merged settings view:
// the highest layer's approval_mode, defaulting to ask for ""/invalid.
func ModeFromSettings(s settings.Settings) Mode {
	if m, ok := ParseMode(s.ApprovalMode); ok {
		return m
	}
	return ModeAsk
}

// ModeFrom unwraps a possibly-decorated approver chain to find the
// ModeApprover. The loop's chain is Allowlist → ModeApprover → human on every
// path, so /auto, /mode, and the goal offer all reach it through the Allowlist.
func ModeFrom(a agent.Approver) (*ModeApprover, bool) {
	switch v := a.(type) {
	case *ModeApprover:
		return v, true
	case *Allowlist:
		return ModeFrom(v.next)
	default:
		return nil, false
	}
}

// ModeApprover implements the three approval tiers. It sits between the
// Allowlist (which owns allow/deny globs — deny wins before this layer is
// consulted) and the terminal human approver (ConfirmApprover on the CLI,
// RemoteApprover over the wire), and it also answers plan approval and
// out-of-workspace path access so the tier governs every human gate uniformly:
//
//	Allowlist(rules) → ModeApprover(mode) → human
//
// The tier is an atomic value because /auto and /mode flip it from the input
// goroutine while a turn may consult it from another — the same shape as
// AutoApprover.enabled — and the change takes effect at the next tool boundary.
type ModeApprover struct {
	mode atomic.Uint32 // 0=ask 1=auto 2=full
	root string        // workspace root; path checks only match inside it
	next agent.Approver
	plan agent.PlanApprover // human plan approver (CLI: the TUI dialog; wire: next)
}

// NewModeApprover wraps the terminal human tool approver with the given tier.
// root is the workspace root ("" disables path checks, so nothing auto-matches
// in auto tier). next may be nil-safe: a nil human denies in ask mode.
func NewModeApprover(mode Mode, root string, next agent.Approver) *ModeApprover {
	a := &ModeApprover{root: root, next: next}
	a.SetMode(mode)
	return a
}

// WithPlanApprover attaches the human plan approver used when the tier
// delegates plan decisions. On the wire this is the same RemoteApprover as
// next; on the CLI it is the TUI's plan dialog, which is a different object.
// When nil, ask mode rejects plans (fail-safe: no human to review them).
func (a *ModeApprover) WithPlanApprover(p agent.PlanApprover) *ModeApprover {
	a.plan = p
	return a
}

// Mode reports the current tier.
func (a *ModeApprover) Mode() Mode {
	switch a.mode.Load() {
	case 1:
		return ModeAuto
	case 2:
		return ModeFull
	default:
		return ModeAsk
	}
}

// SetMode flips the tier live. Used by /auto, /mode, and the wire PUT.
func (a *ModeApprover) SetMode(m Mode) {
	switch m {
	case ModeAuto:
		a.mode.Store(1)
	case ModeFull:
		a.mode.Store(2)
	default:
		a.mode.Store(0)
	}
}

// Approve implements agent.Approver (verdict only, no audit).
func (a *ModeApprover) Approve(toolName string, input json.RawMessage) agent.Verdict {
	v, _ := a.ApproveAudited(toolName, input)
	return v
}

// ApproveAudited implements agent.AuditedApprover. A call the tier grants is
// returned with a human-readable reason so the loop emits a correlated
// EventAutoApproved; everything else delegates to the human (which keeps its
// own audit when it has one). A nil human (headless firing with no connected
// client) denies — the same fail-safe as a nil runner Approver.
func (a *ModeApprover) ApproveAudited(toolName string, input json.RawMessage) (agent.Verdict, string) {
	if reason, ok := a.autoApprove(toolName, input); ok {
		return agent.VerdictAllow, reason
	}
	if a.next == nil {
		return agent.VerdictDeny, ""
	}
	if aa, ok := a.next.(agent.AuditedApprover); ok {
		return aa.ApproveAudited(toolName, input)
	}
	return a.next.Approve(toolName, input), ""
}

// ApprovePlan implements agent.PlanApprover. auto and full approve the plan
// without the human (the user picked the tier); ask delegates to the human
// plan approver, or rejects when none is wired (fail-safe).
func (a *ModeApprover) ApprovePlan(plan agent.Plan) agent.PlanDecision {
	switch a.Mode() {
	case ModeAuto, ModeFull:
		return agent.PlanApproved
	default:
		if a.plan != nil {
			return a.plan.ApprovePlan(plan)
		}
		if pa, ok := a.next.(agent.PlanApprover); ok {
			return pa.ApprovePlan(plan)
		}
		return agent.PlanRejected
	}
}

// ApproveExternalPath implements tools.PathAccessApprover. full auto-allows
// out-of-workspace paths except protected ones (secrets are never auto-
// exposed); auto and ask delegate to the human path approver, or deny when
// none exists (the CLI today, where external paths are rejected outright).
func (a *ModeApprover) ApproveExternalPath(absolutePath string, operation string) bool {
	if a.Mode() == ModeFull {
		return !sandbox.IsPathProtected(absolutePath, sandbox.ProtectedPaths(nil))
	}
	if pa, ok := a.next.(tools.PathAccessApprover); ok {
		return pa.ApproveExternalPath(absolutePath, operation)
	}
	return false
}

// autoApprove grants the call when the current tier covers it, with an audit
// reason. ask delegates everything; auto covers in-workspace writes and
// non-network commands; full covers everything except protected-path writes.
// deny is enforced by the Allowlist before this layer; blocked commands and
// protected redirects are enforced by the tools themselves.
func (a *ModeApprover) autoApprove(toolName string, input json.RawMessage) (string, bool) {
	switch a.Mode() {
	case ModeAuto:
		return a.autoApproveTier(toolName, input)
	case ModeFull:
		return a.fullApprove(toolName, input)
	default:
		return "", false
	}
}

// autoApproveTier is the "auto" policy: in-workspace file writes (edit/create
// with a non-protected target, apply_patch, git_commit), non-network
// run_command, and MCP server tools auto-run; network commands and everything
// else delegate to the human. run_command reaching the approver is by
// construction a Confirm-tier command (Allow-tier commands never gate,
// Block-tier never run) — the network check is what keeps curl / git push /
// npm install human. MCP tools are auto-approved because "auto" means "trust
// the workspace's toolset" to the user; deny rules (Allowlist, outermost)
// still gate specific MCP tools precisely.
func (a *ModeApprover) autoApproveTier(toolName string, input json.RawMessage) (string, bool) {
	switch toolName {
	case "edit_file", "create_file":
		path, ok := decodePath(input)
		if !ok || !a.insideWorkspace(path) {
			return "", false
		}
		if sandbox.IsPathProtected(path, sandbox.ProtectedPaths(nil)) {
			return "", false
		}
		return "approval mode auto: in-workspace " + toolName + " " + path, true
	case "apply_patch":
		// Workspace-relative diff; the tool enforces containment itself.
		return "approval mode auto: in-workspace apply_patch", true
	case "git_commit":
		// In-workspace git operation. Note: runs .git/hooks — accepted as part
		// of the tier (deny/protected rules still gate the dangerous cases).
		return "approval mode auto: in-workspace git_commit", true
	case "run_command":
		cmd, ok := decodeCommand(input)
		if !ok || sandbox.IsNetworkCommand(cmd) {
			return "", false // network commands still ask in auto
		}
		return "approval mode auto: in-workspace command " + cmd, true
	default:
		// MCP server tools auto-run in auto tier (deny rules still gate them).
		if strings.HasPrefix(toolName, "mcp__") {
			return "approval mode auto: mcp tool " + toolName, true
		}
		return "", false // unknown tools ask
	}
}

// fullApprove is the "full" policy: every side-effecting call auto-runs
// except writes to protected paths (a secret file is never mutated silently,
// even in full access — the human must confirm). The wrapped human is
// consulted for those, so the hard line survives.
func (a *ModeApprover) fullApprove(toolName string, input json.RawMessage) (string, bool) {
	switch toolName {
	case "edit_file", "create_file":
		if path, ok := decodePath(input); ok && sandbox.IsPathProtected(path, sandbox.ProtectedPaths(nil)) {
			return "", false
		}
	}
	return "approval mode full: " + toolName, true
}

// insideWorkspace resolves a tool's relative "path" exactly as edit_file /
// create_file do and reports whether it stays inside the workspace root.
// A lexical check — same defense-in-depth role as AutoApprover.insideWorkspace.
func (a *ModeApprover) insideWorkspace(path string) bool {
	return insideRoot(a.root, path)
}

// insideRoot reports whether the target path resolves inside root, resolving
// it exactly as the filesystem tools do in their Execute (Join → Clean →
// Abs → workspace.ValidatePath). A lexical check, shared with AutoApprover.
func insideRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	target := filepath.Join(root, filepath.Clean(path))
	target, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	return workspace.ValidatePath(root, target) == nil
}

// decodeCommand pulls the "command" field from a run_command input. Malformed
// input returns ok=false → the call delegates to the human (fail-safe).
func decodeCommand(input json.RawMessage) (string, bool) {
	if len(input) == 0 {
		return "", false
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", false
	}
	return in.Command, in.Command != ""
}

var (
	_ agent.Approver          = (*ModeApprover)(nil)
	_ agent.AuditedApprover   = (*ModeApprover)(nil)
	_ agent.PlanApprover      = (*ModeApprover)(nil)
	_ tools.PathAccessApprover = (*ModeApprover)(nil)
)
