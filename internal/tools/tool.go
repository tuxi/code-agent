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

	// PlanMode is true when the runner is in a planning/proposing state.
	// Tools that need different behavior during plan mode (e.g. write_file
	// restricting writes to .codeagent/plans/) check this field.
	PlanMode bool

	// OnStdout, if set, receives stdout chunks as the command produces them.
	// Tools that support streaming call this during execution; nil-safe.
	OnStdout func(chunk string)
	// OnStderr, if set, receives stderr chunks as the command produces them.
	// Tools that support streaming call this during execution; nil-safe.
	OnStderr func(chunk string)
}

type ToolResult struct {
	Content string `json:"content"`
}

// ClientProxyTool is a Tool stub registered from a client's register_tools
// message. It provides the tool definition to the model (Name, Description,
// InputSchema) so the model can call it, but Execute is never invoked — the
// agent loop sees ExecStrictClient and delegates to the remote client instead.
type ClientProxyTool struct {
	name        string
	description string
	inputSchema json.RawMessage
}

// NewClientProxyTool creates a client-tool proxy from a wire definition.
func NewClientProxyTool(name, description string, inputSchema json.RawMessage) *ClientProxyTool {
	return &ClientProxyTool{name: name, description: description, inputSchema: inputSchema}
}

func (t *ClientProxyTool) Name() string                 { return t.name }
func (t *ClientProxyTool) Description() string          { return t.description }
func (t *ClientProxyTool) InputSchema() json.RawMessage { return t.inputSchema }
func (t *ClientProxyTool) ExecutionMode() ExecutionMode { return ExecStrictClient }

// Execute is a no-op stub. The agent loop never calls it for ExecStrictClient tools.
func (t *ClientProxyTool) Execute(_ context.Context, _ ExecutionContext, _ json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "tool error: this tool runs on the client"}, nil
}

// compile-time checks
var _ Tool = (*ClientProxyTool)(nil)
var _ ClientTool = (*ClientProxyTool)(nil)

// ExecutionMode declares which side must execute a tool. This is a contract —
// not a suggestion. The server loop and client dispatcher both consult it to
// make non-negotiable decisions about who runs the tool.
//
// See docs/protocols/agent-wire-v1.1-client-tool-execution.md §2.
type ExecutionMode string

const (
	// ExecStrictServer: only the server may execute this tool (grep, git, bash, …).
	ExecStrictServer ExecutionMode = "strict_server"

	// ExecStrictClient: only the client may execute this tool (AVFoundation,
	// HealthKit, Photos, …). The server physically cannot run these.
	ExecStrictClient ExecutionMode = "strict_client"

	// ExecFlex: either side can execute; the server does by default. v2 may
	// negotiate. web_search, web_fetch fall here.
	ExecFlex ExecutionMode = "flex"
)

// ClientTool is an optional interface a tool implements to declare that it must
// be executed by the client. Tools that don't implement it default to
// ExecStrictServer.
type ClientTool interface {
	ExecutionMode() ExecutionMode
}

type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, ec ExecutionContext, input json.RawMessage) (ToolResult, error)
}
