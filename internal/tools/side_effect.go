package tools

import "encoding/json"

// SideEffecting is implemented by tools that change state outside the agent —
// writing files, running commands, anything not purely read-only. The runtime
// gates these behind user confirmation before they run.
//
// It is an optional marker: read-only tools simply do not implement it and are
// treated as safe to run without confirmation.
type SideEffecting interface {
	SideEffects() bool
}

// SideEffectingInput is implemented by tools whose side-effect status depends on
// the *specific* input rather than being fixed per tool. run_command is the
// motivating case: "git status" is read-only and should run without a prompt,
// while "rm build" mutates the tree and must be confirmed. A tool may implement
// both interfaces; the input-aware one takes precedence (see HasSideEffectsFor).
type SideEffectingInput interface {
	SideEffectsFor(input json.RawMessage) bool
}

// HasSideEffects reports whether a tool declares side effects. A tool that does
// not implement SideEffecting is considered read-only.
func HasSideEffects(t Tool) bool {
	se, ok := t.(SideEffecting)
	return ok && se.SideEffects()
}

// HasSideEffectsFor reports whether a specific tool call should be gated behind
// confirmation. It prefers an input-aware decision (SideEffectingInput) and
// falls back to the static per-tool marker, so existing tools keep their
// behavior unchanged while command-aware tools can opt into finer gating.
func HasSideEffectsFor(t Tool, input json.RawMessage) bool {
	if si, ok := t.(SideEffectingInput); ok {
		return si.SideEffectsFor(input)
	}
	return HasSideEffects(t)
}
