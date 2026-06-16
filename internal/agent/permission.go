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

// approve consults the configured Approver. A nil Approver is fail-safe: it
// denies every side-effecting call rather than running it silently.
func (r *Runner) approve(toolName string, input json.RawMessage) bool {
	if r.Approver == nil {
		return false
	}
	return r.Approver.Approve(toolName, input)
}
