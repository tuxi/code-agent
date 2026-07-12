package server

import (
	"encoding/json"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
	"code-agent/internal/model"
)

// Command-plane messages (client -> server): they trigger an action on a session
// and expect no direct reply — the effect shows up on the event stream. The
// control-plane approval messages (request/response RPC) live in control.go.

// Message type discriminators for the command plane.
const (
	MsgTypeSendMessage          = "send_message"
	MsgTypeCancelTurn           = "cancel_turn"
	MsgTypePlanApprovalResponse = "plan_approval_response"
	MsgTypeAgentInput           = "agent_input"    // v1.1 unified inbound envelope
	MsgTypeRegisterTools        = "register_tools" // v1.1 client tool registration
	MsgTypeInvokePrompt         = "invoke_prompt"  // invoke an MCP prompt (server renders → runs a turn)
)

// InvokePrompt asks the server to render an MCP prompt template and run the
// result as a turn. Command is the prompt's command name (mcp__<server>__<prompt>,
// as listed by GET /v1/prompts); Args are positional, mapped onto the prompt's
// declared argument order. The rendered turn's output flows on the normal event
// stream — there is no direct reply.
type InvokePrompt struct {
	Type    string   `json:"type"` // always "invoke_prompt"
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// SendMessage drives one turn. Text is the user input.
type SendMessage struct {
	Type string `json:"type"` // always "send_message"
	Text string `json:"text"`
}

// CancelTurn cancels the in-flight turn at the next checkpoint.
type CancelTurn struct {
	Type string `json:"type"` // always "cancel_turn"
}

// RegisterTools is sent by the client after the hello handshake to declare
// which tools it can execute. Each tool becomes a ClientTool proxy on the
// server side — the server advertises it to the model but never executes it
// locally. See docs/protocols/agent-wire-v1.1-client-tool-execution.md.
type RegisterTools struct {
	Type  string                `json:"type"` // always "register_tools"
	Tools []agent.ClientToolDef `json:"tools"`
}

// AgentInput is the v1.1 unified inbound envelope. Clients send this instead of
// the v1 send_message / cancel_turn flat types. The Router dispatches by Kind.
// See docs/protocols/agent-wire-v1.1-client-tool-execution.md §1.
type AgentInput struct {
	Type       string                  `json:"type"`                  // always "agent_input"
	Kind       string                  `json:"kind"`                  // "text" | "tool_result" | "command" | "system"
	Text       string                  `json:"text,omitempty"`        // kind="text" | "command"
	Model      string                  `json:"model,omitempty"`       // optional: model profile name to use this turn
	Assets     []model.GatewayAssetRef `json:"assets,omitempty"`      // kind="text": Gateway-owned user asset refs
	ToolResult *ToolResult             `json:"tool_result,omitempty"` // kind="tool_result"
	// kind="system" fields (v1.1 parses but stubs out the semantics):
	Command      string `json:"command,omitempty"`
	CommandKey   string `json:"command_key,omitempty"`
	CommandValue string `json:"command_value,omitempty"`
}

// ToolResult is the client-tool-execution result payload carried inside an
// AgentInput of kind "tool_result". It maps 1:1 to the execution graph edge
// identified by tool_use_id. See §3 of the v1.1 spec.
type ToolResult struct {
	ToolUseID string          `json:"tool_use_id"`
	Subtype   string          `json:"subtype"` // "result" (v1.1); "progress"|"error"|"cancel" (v1.2+)
	Content   string          `json:"content,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Assets    []assets.Ref    `json:"assets,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}
