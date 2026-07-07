// Package approve holds the auto-approval policy that decides, when auto mode is
// on, whether a side-effecting tool call may run without a human prompt. It is a
// drop-in agent.Approver: the loop's call path is unchanged — it still consults
// the Approver only for side-effecting calls (tools.HasSideEffectsFor), and a
// disabled AutoApprover behaves byte-for-byte like the wrapped human approver, so
// wiring it in changes nothing until the user opts in with `/auto on` or `--auto`.
//
// The contract implemented here is locked in docs/p9.1-code-agent-auto-mode.md
// §12. Two load-bearing rules:
//
//   - Fail-safe to human: anything the policy does not POSITIVELY classify as safe
//     falls back to the human approver. It is never auto-denied and never silently
//     auto-approved (mirrors the loop's "nil Approver → deny" direction).
//   - Trusted inputs only: the policy is built from the workspace root and
//     hard-coded rules. It never reads repo content or tool output to decide, and
//     the enable switch is flipped only by `/auto` / `--auto` — so a
//     prompt-injection in a file the agent read cannot widen what auto-approves.
//
// Auditing is delegated to the loop via agent.AuditedApprover: this type reports
// the auto-grant reason and the loop emits the correlated, persisted event (it
// holds the session/turn IDs an audit record needs). See ApproveAudited.
package approve

import (
	"encoding/json"
	"path/filepath"
	"sync/atomic"

	"code-agent/internal/agent"
	"code-agent/internal/sandbox"
	"code-agent/internal/workspace"
)

// AutoApprover is the policy-driven agent.Approver. When disabled it delegates
// every call to Human (identical to today). When enabled it auto-approves only the
// narrow set §12.2 locks for Phase 1 — in-workspace edit_file / create_file — and
// falls back to Human for everything else: all run_command (Confirm-tier ≈
// never-auto), apply_patch, git_commit, and any out-of-workspace write.
type AutoApprover struct {
	// Human is the fallback approver consulted whenever the policy does not
	// positively auto-approve. In practice a terminal ui.ConfirmApprover.
	Human agent.Approver

	// root is the absolute workspace root. Path-based auto-approval only fires for
	// targets that resolve inside it. Computed once at construction from a trusted
	// source (config/CLI), never from anything the agent can write.
	root string

	// enabled is the session-level switch, flipped by `/auto on|off` and seeded by
	// `--auto`. Atomic because the REPL toggles it from the input goroutine while a
	// turn may consult it from another; the kill switch then takes effect at the
	// next tool boundary (the loop consults the approver before each tool call,
	// never mid-call).
	enabled atomic.Bool
}

// NewAutoApprover wraps a human approver with the auto policy. enabled seeds the
// initial state (from --auto); /auto flips it later. The workspace root is
// absolutized once; if that fails it is kept as-is and path checks simply never
// match (fail-safe to human).
func NewAutoApprover(workspaceRoot string, human agent.Approver, enabled bool) *AutoApprover {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		root = workspaceRoot
	}
	a := &AutoApprover{Human: human, root: root}
	a.enabled.Store(enabled)
	return a
}

// SetEnabled flips the session-level switch. Used by the `/auto on|off` command.
func (a *AutoApprover) SetEnabled(on bool) { a.enabled.Store(on) }

// Enabled reports the current switch state (for `/auto` with no argument).
func (a *AutoApprover) Enabled() bool { return a.enabled.Load() }

// Approve implements agent.Approver. It is the verdict-only path for callers that
// do not audit; the loop uses ApproveAudited so auto-grants are logged.
func (a *AutoApprover) Approve(toolName string, input json.RawMessage) agent.Verdict {
	v, _ := a.ApproveAudited(toolName, input)
	return v
}

// ApproveAudited implements agent.AuditedApprover. Disabled → delegate to the
// human (today's behavior), no audit. Enabled → auto-approve the locked safe set,
// returning a reason for the loop to emit a correlated audit event; otherwise
// fail-safe to the human prompt (no audit — a human saw it).
func (a *AutoApprover) ApproveAudited(toolName string, input json.RawMessage) (agent.Verdict, string) {
	if !a.enabled.Load() {
		return a.Human.Approve(toolName, input), ""
	}
	if reason, ok := a.autoApprove(toolName, input); ok {
		return agent.VerdictAllow, reason
	}
	return a.Human.Approve(toolName, input), ""
}

// autoApprove reports whether the locked Phase 1 policy (§12.2) auto-grants this
// call, with a human-readable reason for the audit event. Anything it does not
// positively match returns ok=false → the caller asks the human.
//
// Phase 1 surface is intentionally tiny: edit_file and create_file whose target
// path resolves inside the workspace root. Both tools name the target "path", so
// one decode covers both. run_command (any Confirm-tier command), apply_patch, and
// git_commit are deliberately absent — they stay human (§12.2 / ADR-9; git_commit
// because it runs arbitrary .git/hooks, apply_patch because its multi-file path
// set is not statically checkable without parsing the diff).
func (a *AutoApprover) autoApprove(toolName string, input json.RawMessage) (string, bool) {
	switch toolName {
	case "edit_file", "create_file":
		path, ok := decodePath(input)
		if !ok || path == "" {
			return "", false // can't see the target → fail-safe to human
		}
		if !a.insideWorkspace(path) {
			return "", false // out-of-workspace write → human (the tool also refuses)
		}
		// Protected paths (P3+P4): .env, credentials, keys, etc. — never auto-
		// approved. The human must explicitly confirm writes to these files.
		if sandbox.IsPathProtected(path, sandbox.ProtectedPaths(nil)) {
			return "", false // protected path → human must confirm
		}
		return "workspace-internal write: " + path, true
	default:
		return "", false
	}
}

// insideWorkspace resolves a tool's relative "path" exactly as edit_file /
// create_file do in their Execute (filepath.Join(root, Clean(path)) → Abs →
// workspace.IsSubPath) and reports whether it stays inside the workspace. This is a
// defense-in-depth pre-check: the tool independently re-runs the identical check
// and refuses an escape, so even a mismatch here cannot write outside the root.
//
// NOTE: like the tools, this is a LEXICAL check — it does not resolve symlinks, so
// a symlink inside the workspace pointing out is not caught here (nor by the tools
// today). Tracked as Phase 2 hardening (§12.7); auto mode follows current tool
// behavior for Phase 1.
func (a *AutoApprover) insideWorkspace(path string) bool {
	target := filepath.Join(a.root, filepath.Clean(path))
	target, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	return workspace.IsSubPath(a.root, target)
}

// decodePath pulls the "path" field from a tool input. edit_file and create_file
// both use it as the target file relative to the workspace root. A malformed input
// returns ok=false → fail-safe to human (Execute will surface the real error).
func decodePath(input json.RawMessage) (string, bool) {
	if len(input) == 0 {
		return "", false
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", false
	}
	return in.Path, true
}

var (
	_ agent.Approver        = (*AutoApprover)(nil)
	_ agent.AuditedApprover = (*AutoApprover)(nil)
)
