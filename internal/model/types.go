package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyAssistantResponse indicates that a provider ended a response without
// either user-visible text or tool calls. Such a response cannot be represented
// as a valid assistant message in OpenAI-compatible conversation history.
var ErrEmptyAssistantResponse = errors.New("model returned an empty assistant response")

type Usage struct {
	PromptTokens     int   `json:"prompt_tokens"`
	CompletionTokens int   `json:"completion_tokens"`
	TotalTokens      int   `json:"total_tokens"`
	BillingUnits     int64 `json:"billing_units,omitempty"`

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

	// Assets are Gateway-owned, ownership-checked asset references. They contain
	// neither bytes nor OSS URLs and are safe to persist with the message.
	Assets []GatewayAssetRef `json:"assets,omitempty"`

	// ToolCalls is set on assistant messages when the model decides to call one
	// or more tools. It must be echoed back unchanged in the next request.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID is set on tool-result messages (Role == RoleTool) to bind the
	// result to the assistant tool call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// GatewayAssetRef is the minimal asset-first contract sent to Agent Gateway.
// It is intentionally distinct from assetref.Ref, which is a local Runtime/UI
// index and can contain paths or previews that must never reach Gateway.
type GatewayAssetRef struct {
	AssetID  int64  `json:"asset_id"`
	SHA256   string `json:"sha256,omitempty"`
	Kind     string `json:"kind"`
	MIMEType string `json:"mime_type"`
	Filename string `json:"filename"`
}

// ToolCall is a single function call the model requested.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // currently always "function"
	Function FunctionCall `json:"function"`
}

func (c ToolCall) ValidateForHistory() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("tool call id is empty")
	}

	if c.Type != "function" {
		return fmt.Errorf("unsupported tool call type %q", c.Type)
	}

	if strings.TrimSpace(c.Function.Name) == "" {
		return errors.New("tool function name is empty")
	}

	var arguments any
	if err := json.Unmarshal(
		[]byte(c.Function.Arguments),
		&arguments,
	); err != nil {
		return fmt.Errorf("function arguments are invalid JSON: %w", err)
	}

	if _, ok := arguments.(map[string]any); !ok {
		return errors.New("function arguments must be a JSON object")
	}

	return nil
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
	// SessionID and ExecutionID are Gateway correlation identifiers. They are
	// carried on the request envelope, never inserted into model-visible text.
	SessionID   string    `json:"session_id,omitempty"`
	ExecutionID string    `json:"execution_id,omitempty"`
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

// ValidateAssistantTurn verifies that this response can safely be persisted as
// an assistant message. Tool-only responses are valid, but a response with
// neither text nor tool calls is an invalid no-op and must fail the turn rather
// than poison the next provider request.
func (r Response) ValidateAssistantTurn() error {
	if strings.TrimSpace(r.Content) == "" && !r.HasToolCalls() {
		return ErrEmptyAssistantResponse
	}

	for i, call := range r.ToolCalls {
		if err := call.ValidateForHistory(); err != nil {
			return fmt.Errorf(
				"tool call %d (%q) is invalid: %w",
				i,
				call.Function.Name,
				err,
			)
		}
	}
	return nil
}

// IsEmptyAssistantNoOp reports whether a persisted message is an assistant
// no-op that OpenAI-compatible providers reject. It is intentionally limited to
// assistant messages, so empty tool-call turns and empty tool results retain
// their protocol semantics.
func (m Message) IsEmptyAssistantNoOp() bool {
	return m.Role == RoleAssistant && strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0
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

// StreamingProvider is an OPTIONAL capability on top of Provider: a provider that
// can stream the model's text content as it is generated, calling onText for each
// content delta, while still returning the SAME complete Response that Complete
// would. Tool-call deltas are accumulated internally (the loop needs them whole),
// so only human-read text streams. Callers type-assert for it and fall back to
// Complete when absent — so Complete stays the contract everything else depends on.
type StreamingProvider interface {
	CompleteStream(ctx context.Context, request Request, onText func(delta string)) (Response, error)
}
