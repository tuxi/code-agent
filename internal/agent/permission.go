package agent

import "encoding/json"

// Verdict is a three-state permission decision, replacing the bool return of the
// old Approver interface. It gives each layer in the chain the power to signal
// "delegate to the next layer" without conflating it with a denial.
type Verdict int

const (
	// VerdictAllow auto-approves the call — short-circuit, skip remaining layers
	// and the human prompt.
	VerdictAllow Verdict = iota
	// VerdictDeny refuses the call outright. No human is consulted, and the model
	// sees a "denied by policy" message (not "user declined").
	VerdictDeny
	// VerdictAsk delegates the decision to the next layer. It is the policy
	// layer's way of saying "I have no opinion, ask someone else." Terminal human
	// approvers NEVER return Ask — they are the end of the chain and return only
	// Allow or Deny. If a terminal approver returns Ask anyway, the loop treats
	// it as Deny (fail-safe).
	VerdictAsk
)

// Approver gates side-effecting tool calls. The loop consults it before running
// any tool that reports side effects (file writes, command execution). Read-only
// tools never reach it.
//
// Each implementation returns a Verdict:
//   - Policy layers (Allowlist, AutoApprover) may return Allow, Deny, or Ask.
//   - Terminal human approvers (ConfirmApprover, tuiApprover, RemoteApprover)
//     return only Allow or Deny — they are the end of the chain and never Ask.
type Approver interface {
	// Approve returns the permission verdict for a side-effecting tool call. The
	// input is the raw tool arguments, so an implementation can show the user
	// exactly what is about to happen.
	Approve(toolName string, input json.RawMessage) Verdict
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
	ApproveAudited(toolName string, input json.RawMessage) (Verdict, string)
}

// approve consults the configured Approver. A nil Approver is fail-safe: it
// denies every side-effecting call rather than running it silently. When the
// approver is an AuditedApprover and it auto-granted the call, the loop emits a
// correlated EventAutoApproved so the grant is visible and durably logged.
func (r *Runner) approve(toolName string, input json.RawMessage) Verdict {
	if r.Approver == nil {
		return VerdictDeny
	}
	if aa, ok := r.Approver.(AuditedApprover); ok {
		verdict, autoReason := aa.ApproveAudited(toolName, input)
		if verdict == VerdictAllow && autoReason != "" {
			r.emit(Event{
				Kind:     EventAutoApproved,
				ToolName: toolName,
				ToolArgs: string(input),
				Text:     autoReason,
			})
		}
		return verdict
	}
	return r.Approver.Approve(toolName, input)
}
