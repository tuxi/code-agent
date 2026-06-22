package agent

import "encoding/json"

// Approver gates side-effecting tool calls. The loop consults it before running
// any tool that reports side effects (file writes, command execution). Read-only
// tools never reach it.
//
// This is the seed of the permission/policy layer. Today there is a single CLI
// implementation, but the loop depends only on this interface, so a richer
// policy (allowlists, per-path rules, auto-approve modes) can replace it later
// without touching the loop.
type Approver interface {
	// Approve returns whether the side-effecting tool call may proceed. The
	// input is the raw tool arguments, so an implementation can show the user
	// exactly what is about to happen.
	Approve(toolName string, input json.RawMessage) bool
}

// AuditedApprover is an optional extension of Approver for approvers that can
// AUTO-grant a call without a human prompt (auto mode). It lets the loop emit a
// correlated, persisted audit event for each auto-grant, so an unattended run can
// be reviewed afterward ("what was approved without me, and why").
//
// The reason it lives here rather than in the auto-approver itself: the audit
// event must carry the session/turn IDs to be retrievable per run, and only the
// loop holds them (it stamps every event via r.emit). An approver emitting
// directly would produce uncorrelated events. This stays a drop-in extension —
// the plain Approver path is unchanged and approvers that don't implement it (the
// terminal prompt, the TUI) need no audit, since a human saw every decision. It
// mirrors the loop's other optional interfaces (SkillAnnouncer, SideEffectingInput).
type AuditedApprover interface {
	Approver
	// ApproveAudited returns the verdict and, when the call was auto-granted with
	// no human prompt, a non-empty human-readable reason for the audit event. The
	// reason is empty when the verdict came from a human or was a denial.
	ApproveAudited(toolName string, input json.RawMessage) (approved bool, autoReason string)
}

// approve consults the configured Approver. A nil Approver is fail-safe: it
// denies every side-effecting call rather than running it silently. When the
// approver is an AuditedApprover and it auto-granted the call, the loop emits a
// correlated EventAutoApproved so the grant is visible and durably logged.
func (r *Runner) approve(toolName string, input json.RawMessage) bool {
	if r.Approver == nil {
		return false
	}
	if aa, ok := r.Approver.(AuditedApprover); ok {
		approved, autoReason := aa.ApproveAudited(toolName, input)
		if approved && autoReason != "" {
			r.emit(Event{
				Kind:     EventAutoApproved,
				ToolName: toolName,
				ToolArgs: string(input),
				Text:     autoReason,
			})
		}
		return approved
	}
	return r.Approver.Approve(toolName, input)
}
