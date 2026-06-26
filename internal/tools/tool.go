package tools

import (
	"context"
	"encoding/json"
)

// ExecutionContext carries the runtime state for a single tool execution.
// It is a value type — tools receive it by value and must not mutate it.
// All fields are immutable for the duration of the Execute call.
type ExecutionContext struct {
	// WorkspaceRoot is the absolute path of the project root directory.
	// Tools use it to resolve relative paths and enforce workspace boundaries.
	WorkspaceRoot string

	// SessionID is the stable conversation identifier (the session UUID).
	SessionID string

	// TurnID is the per-turn identifier (e.g. "turn_5").
	TurnID string

	// CallID is the per-tool-call identifier (e.g. "call_3").
	CallID string
}

type ToolResult struct {
	Content string `json:"content"`
}

type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, ec ExecutionContext, input json.RawMessage) (ToolResult, error)
}
