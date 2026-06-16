package tools

// SideEffecting is implemented by tools that change state outside the agent —
// writing files, running commands, anything not purely read-only. The runtime
// gates these behind user confirmation before they run.
//
// It is an optional marker: read-only tools simply do not implement it and are
// treated as safe to run without confirmation.
type SideEffecting interface {
	SideEffects() bool
}

// HasSideEffects reports whether a tool declares side effects. A tool that does
// not implement SideEffecting is considered read-only.
func HasSideEffects(t Tool) bool {
	se, ok := t.(SideEffecting)
	return ok && se.SideEffects()
}
