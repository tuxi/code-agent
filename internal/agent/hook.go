package agent

import (
	"context"
	"encoding/json"
)

// ToolHook runs user-configured commands around tool execution (8.5) — the same
// nil-safe, loop-stays-tool-agnostic pattern as Approver (consulted before a tool)
// and Observer (after). Its point is DETERMINISM: a hook runs every time,
// regardless of the model, which is exactly what soft, model-dependent behavior
// (skills, todo, delegation) cannot guarantee.
//
// Defined here so the loop depends only on the interface; the implementation,
// which runs shell commands, lives in internal/hooks.
type ToolHook interface {
	// PreToolUse runs before a tool executes. A non-nil error BLOCKS the call: the
	// loop reports it and does not run the tool — a hard guardrail (e.g. refuse a
	// destructive command), not a request.
	PreToolUse(ctx context.Context, tool string, input json.RawMessage) error
	// PostToolUse runs after a tool succeeds — e.g. format or lint the change. It
	// is best-effort: a failure is logged but does not undo the tool or alter its
	// result (v1 does not amend results).
	PostToolUse(ctx context.Context, tool string, input json.RawMessage, result string) error
}

// preHookBlock consults the PreToolUse hook, returning a non-empty reason when the
// call is blocked. Nil-safe: no hook means never blocked.
func (r *Runner) preHookBlock(ctx context.Context, tool string, input json.RawMessage) string {
	if r.Hook == nil {
		return ""
	}
	if err := r.Hook.PreToolUse(ctx, tool, input); err != nil {
		return err.Error()
	}
	return ""
}

// postHook consults the PostToolUse hook, best-effort. Nil-safe.
func (r *Runner) postHook(ctx context.Context, tool string, input json.RawMessage, result string) {
	if r.Hook == nil {
		return
	}
	_ = r.Hook.PostToolUse(ctx, tool, input, result)
}
