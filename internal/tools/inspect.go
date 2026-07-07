package tools

import "encoding/json"

// Inspector is an optional interface a Tool may implement to perform a static
// safety validation before execution. It runs BEFORE the permission/approval
// gate and BEFORE the tool executes — purely static analysis, no I/O, no
// policy enforcement, no human prompt.
//
// A blocked call is refused outright; the model sees the reason so it can
// choose a different approach. Tools that do not implement Inspector skip this
// check entirely (the existing behavior is unchanged).
//
// This mirrors Claude Code's tool.validateInput() stage in the inspection
// pipeline (stage 4 of the 12-stage tool execution lifecycle).
//
// An Inspector should be fail-safe when the input cannot be parsed: return nil
// and let Execute surface the real error rather than guessing from a partial
// parse. The inspect check is a defense-in-depth layer, not a replacement for
// Execute's own validation.
type Inspector interface {
	// Inspect validates input against tool-specific safety invariants.
	// workspaceRoot is the absolute project root for path resolution.
	// Return nil to pass; a non-nil error blocks the call with the error
	// message surfaced to the model.
	Inspect(input json.RawMessage, workspaceRoot string) error
}

// HasInspector reports whether a tool implements the Inspector interface.
// It follows the same pattern as HasSideEffects: an absent interface means the
// tool has no static safety checks and the call proceeds unchanged.
func HasInspector(t Tool) bool {
	_, ok := t.(Inspector)
	return ok
}
