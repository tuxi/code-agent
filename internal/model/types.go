package model

import (
	"context"
	"encoding/json"
)

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// CachedPromptTokens is the portion of PromptTokens served from the provider's
	// prompt cache (a stable prefix billed at a lower rate). Parsed per-provider by
	// the openai adapter (deepseek's prompt_cache_hit_tokens / OpenAI's
	// prompt_tokens_details.cached_tokens). 0 when the provider reports nothing.
	// It is a breakdown of PromptTokens, NOT additional tokens.
	CachedPromptTokens int `json:"-"`
}

type Role string

const (
	RoleUser      Role = "user"
	RoleSystem    Role = "system"
	RoleAssistant Role = "assistant"
	// RoleTool carries the result of a tool call back to the model. A tool
	// message must set ToolCallID to bind it to the assistant tool call it
	// answers.
	RoleTool Role = "tool"
)

// Message is both the public conversation type and the wire format sent to the
// OpenAI-compatible API. The JSON tags match the API schema directly.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`

	// ToolCalls is set on assistant messages when the model decides to call one
	// or more tools. It must be echoed back unchanged in the next request.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID is set on tool-result messages (Role == RoleTool) to bind the
	// result to the assistant tool call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall is a single function call the model requested.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // currently always "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the tool name and its arguments.
//
// IMPORTANT: Arguments is a JSON-encoded *string* on the wire
// (e.g. "{\"path\":\".\"}"), not a nested object. Parse it with
// json.Unmarshal([]byte(call.Function.Arguments), &v). The provider may emit
// invalid JSON or hallucinated fields, so always validate before executing.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition describes a tool exposed to the model. Parameters must be a
// JSON Schema object describing the tool's input.
type ToolDefinition struct {
	Type     string       `json:"type"` // currently always "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Request struct {
	Messages    []Message `json:"messages"`
	Model       string    `json:"model"`
	Temperature float64   `json:"temperature,omitempty"`

	// Tools is the set of tools the model may call this turn. When empty, the
	// request behaves exactly like the old text-only completion.
	Tools []ToolDefinition `json:"tools,omitempty"`

	// ToolChoice is optional: "auto" (the default when tools are present),
	// "none", or "required". Leave empty to use the provider default.
	ToolChoice string `json:"tool_choice,omitempty"`
}

type Response struct {
	// Content is the assistant's text. It may be empty when the model only
	// returns tool calls.
	Content string `json:"content"`

	// ToolCalls is non-empty when the model wants the runtime to execute tools
	// before continuing.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// FinishReason is the raw provider stop reason ("stop", "tool_calls", ...).
	FinishReason string `json:"finish_reason,omitempty"`

	// 读 API 响应里的 usage，作为 token 计量
	Usage Usage `json:"usage,omitempty"`

	Raw []byte `json:"raw,omitempty"`
}

// HasToolCalls reports whether the model requested any tool execution.
func (r Response) HasToolCalls() bool {
	return len(r.ToolCalls) > 0
}

// AssistantMessage converts the response into the assistant message that must
// be appended to the conversation history before sending tool results back.
// The tool_calls are preserved verbatim, which the API requires.
func (r Response) AssistantMessage() Message {
	return Message{
		Role:      RoleAssistant,
		Content:   r.Content,
		ToolCalls: r.ToolCalls,
	}
}

type Provider interface {
	Complete(ctx context.Context, request Request) (Response, error)
}
