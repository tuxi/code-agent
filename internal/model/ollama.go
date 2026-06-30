package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// OllamaProvider speaks Ollama's native /api/chat protocol. It implements both
// Provider and StreamingProvider, matching the same interfaces as
// OpenAICompatibleProvider so the rest of the stack (ResilientProvider, agent
// loop, cost telemetry) works unchanged.
//
// Compared to routing through Ollama's OpenAI-compatible /v1 endpoint, the
// native provider:
//   - Sets keep_alive to hold the model in GPU memory between agent turns,
//     eliminating per-turn load costs (several seconds on Apple Silicon).
//   - Passes Ollama-specific options (num_ctx, temperature) directly.
//   - Reports Ollama diagnostic metrics (prompt_eval_count, eval_count,
//     load_duration) as Usage tokens.
//   - Normalises the tool-call arguments field, which Ollama may return as
//     either a JSON object or a JSON-encoded string depending on the model.
type OllamaProvider struct {
	BaseURL    string // default: http://localhost:11434
	HTTPClient *http.Client
}

// NewOllamaProvider returns a provider wired to baseURL (defaults to the
// standard Ollama listen address when empty). No API key is required — Ollama
// listens on localhost with no auth by default.
func NewOllamaProvider(baseURL string) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
				// No ResponseHeaderTimeout: Ollama is a local server, and
				// model loading (10–30 s for an 18 GB model) + prompt
				// eval can easily exceed 60 s on first call or with a
				// large context. The total attempt time is bounded by
				// ResilientProvider's context deadline
				// (request_timeout_seconds). A separate header timeout
				// here would only produce false-positive timeouts for
				// normal local-model latency.
				ExpectContinueTimeout: 1 * time.Second,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// ── Ollama native API schemas ───────────────────────────────────────────

// ollamaChatRequest is the body for POST /api/chat.
// It uses ollamaMessage (not Message) because Ollama's native API expects
// tool-call arguments as a JSON object, whereas the canonical Message type
// stores them as a JSON-encoded string (OpenAI wire format).
type ollamaChatRequest struct {
	Model     string           `json:"model"`
	Messages  []ollamaMessage  `json:"messages"`
	Stream    bool             `json:"stream"`
	Tools     []ToolDefinition `json:"tools,omitempty"`
	KeepAlive string           `json:"keep_alive,omitempty"`
	Options   ollamaOptions    `json:"options,omitempty"`
}

// ollamaMessage is the per-message shape Ollama's /api/chat expects.
// The only difference from Message is that tool-call arguments are
// json.RawMessage — they must be a JSON object, not a JSON-encoded string.
type ollamaMessage struct {
	Role       Role            `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// toOllamaMessages converts canonical Messages to the shape Ollama's native
// /api/chat expects. The key transformation: Function.Arguments (a JSON string
// in Message) is parsed back to a JSON value so Ollama receives a proper
// nested object rather than a doubly-encoded string.
func toOllamaMessages(msgs []Message) []ollamaMessage {
	out := make([]ollamaMessage, len(msgs))
	for i, m := range msgs {
		om := ollamaMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			otc := ollamaToolCall{ID: tc.ID, Type: tc.Type}
			otc.Function.Name = tc.Function.Name
			if tc.Function.Arguments != "" {
				// Arguments is a JSON string (e.g. `{"path":"."}`).
				// Send it as a JSON value so Ollama sees a nested object.
				otc.Function.Arguments = json.RawMessage(tc.Function.Arguments)
			}
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		out[i] = om
	}
	return out
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumCtx      int     `json:"num_ctx,omitempty"`
}

// ollamaChatResponse is one JSON object from Ollama's /api/chat. In
// non-streaming mode it is the only object. In streaming mode each NDJSON
// line matches this shape; the final one has Done=true and includes the
// usage fields.
type ollamaChatResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	} `json:"message"`
	Done            bool `json:"done"`
	TotalDuration   int  `json:"total_duration"`
	LoadDuration    int  `json:"load_duration"`
	PromptEvalCount int  `json:"prompt_eval_count"`
	EvalCount       int  `json:"eval_count"`
}

// ollamaToolCall mirrors ToolCall but captures arguments as json.RawMessage so
// it accepts both a JSON object (Ollama native) and a JSON-encoded string
// (some models / OpenAI compatibility shim). toToolCall normalises to the
// canonical wire format (Arguments is always a JSON string).
type ollamaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// toToolCall converts an Ollama tool call to our canonical ToolCall.
// The agent loop expects Function.Arguments to be a JSON-encoded string,
// so a JSON object from Ollama is left as-is (it IS the encoded form) and
// a doubly-encoded string is unquoted.
func (tc ollamaToolCall) toToolCall() ToolCall {
	raw := tc.Function.Arguments
	if len(raw) == 0 {
		return ToolCall{ID: tc.ID, Type: tc.Type, Function: FunctionCall{Name: tc.Function.Name}}
	}
	args := string(raw)
	// If the raw JSON starts with '"' it is a JSON string literal
	// (doubly-encoded); strip the outer quotes. If it starts with '{' it is a
	// JSON object — already the canonical form.
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			args = s
		}
	}
	return ToolCall{
		ID:   tc.ID,
		Type: tc.Type,
		Function: FunctionCall{
			Name:      tc.Function.Name,
			Arguments: args,
		},
	}
}

// ── Qwen-format tool-call fallback ──────────────────────────────────────

// qwenXMLToolCallRE matches the XML-like function-call format that Qwen
// models (qwen3-coder, qwen2.5-coder) emit when the Ollama template does not
// include tool-calling instructions (<|im_start|> / <|tool_calls_section|>
// markers). The model falls back to its training format:
//
//	<function=plan_workflow>
//	<parameter=goal>
//	多行参数值...
//	</parameter>
//	</function>
//
// When the native tool_calls array is empty but the content matches this
// pattern, we parse it and synthesise proper ToolCall structs so the agent
// loop still dispatches tool execution. This is a stop-gap: the permanent fix
// is an Ollama modelfile with a proper Qwen 3 chat template that includes
// tool-calling markers (see docs/ollama-qwen3-tool-calling.md).
var qwenFuncStart = "<function="
var qwenParamStart = "<parameter="

// parseQwenXMLToolCalls extracts tool calls from Qwen's XML-format content.
// Returns nil when the content does not match the expected format.
func parseQwenXMLToolCalls(content string) []ToolCall {
	s := strings.TrimSpace(content)
	if !strings.HasPrefix(s, qwenFuncStart) {
		return nil
	}

	// Split on <function= lines — each is one call.
	var calls []ToolCall
	parts := strings.Split(s, qwenFuncStart)
	for _, part := range parts[1:] { // first element is empty (before first <function=)
		tc := parseOneQwenFunc(strings.TrimSpace(part))
		if tc.Function.Name != "" {
			calls = append(calls, tc)
		}
	}
	return calls
}

func parseOneQwenFunc(block string) ToolCall {
	// block: "plan_workflow>\n<parameter=goal>\nvalue\n</parameter>\n</function>"
	idx := strings.Index(block, ">")
	if idx < 0 {
		return ToolCall{}
	}
	name := strings.TrimSpace(block[:idx])
	rest := block[idx+1:]

	params := map[string]string{}
	for {
		pi := strings.Index(rest, qwenParamStart)
		if pi < 0 {
			break
		}
		rest = rest[pi+len(qwenParamStart):] // "goal>\nvalue\n</parameter>..."

		eq := strings.Index(rest, ">")
		if eq < 0 {
			break
		}
		paramName := strings.TrimSpace(rest[:eq])
		rest = rest[eq+1:]

		endTag := "</parameter>"
		end := strings.Index(rest, endTag)
		if end < 0 {
			break
		}
		paramValue := strings.TrimSpace(rest[:end])
		rest = rest[end+len(endTag):]
		params[paramName] = paramValue
	}

	args, _ := json.Marshal(params)
	return ToolCall{
		ID:   name, // use function name as call id
		Type: "function",
		Function: FunctionCall{
			Name:      name,
			Arguments: string(args),
		},
	}
}

// ── Provider interface ───────────────────────────────────────────────────

// Complete sends a non-streaming chat request to Ollama's /api/chat.
func (p *OllamaProvider) Complete(ctx context.Context, req Request) (Response, error) {
	body := ollamaChatRequest{
		Model:     req.Model,
		Messages:  toOllamaMessages(req.Messages),
		Tools:     req.Tools,
		KeepAlive: "5m",
		Options: ollamaOptions{
			Temperature: req.Temperature,
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	// 10 MiB cap — a single-turn response should never approach this.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return Response{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, &APIError{StatusCode: resp.StatusCode, Body: string(raw)}
	}

	var or ollamaChatResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return Response{}, fmt.Errorf("decode ollama response: %w; raw=%s", err, string(raw))
	}

	toolCalls := make([]ToolCall, len(or.Message.ToolCalls))
	for i, tc := range or.Message.ToolCalls {
		toolCalls[i] = tc.toToolCall()
	}

	// Fallback: Qwen models with an empty / minimal Ollama template may
	// output function calls as XML text in content instead of native
	// tool_calls. Parse and promote to proper tool calls.
	if len(toolCalls) == 0 && strings.Contains(or.Message.Content, qwenFuncStart) {
		if parsed := parseQwenXMLToolCalls(or.Message.Content); len(parsed) > 0 {
			toolCalls = parsed
		}
	}

	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}

	return Response{
		Content:      strings.TrimSpace(or.Message.Content),
		ToolCalls:    toolCalls,
		FinishReason: finish,
		Usage: Usage{
			PromptTokens:     or.PromptEvalCount,
			CompletionTokens: or.EvalCount,
			TotalTokens:      or.PromptEvalCount + or.EvalCount,
		},
		Raw: raw,
	}, nil
}

// ── StreamingProvider interface ──────────────────────────────────────────

// CompleteStream sends a streaming chat request to Ollama's /api/chat.
// Ollama's native streaming emits NDJSON where each line carries the next
// token(s) as a content delta (NOT accumulated text). Each delta is passed
// to onText as-is and accumulated into the final Response.Content.
func (p *OllamaProvider) CompleteStream(ctx context.Context, req Request, onText func(string)) (Response, error) {
	body := ollamaChatRequest{
		Model:     req.Model,
		Messages:  toOllamaMessages(req.Messages),
		Stream:    true,
		Tools:     req.Tools,
		KeepAlive: "5m",
		Options: ollamaOptions{
			Temperature: req.Temperature,
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return Response{}, &APIError{StatusCode: resp.StatusCode, Body: string(raw)}
	}

	var (
		content   strings.Builder
		toolCalls []ToolCall
		usage     Usage
	)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var chunk ollamaChatResponse
		if json.Unmarshal([]byte(line), &chunk) != nil {
			continue
		}

		// Ollama's native streaming sends content deltas (not accumulated
		// text). Each chunk IS the next token/word — pass it directly.
		if chunk.Message.Content != "" {
			delta := chunk.Message.Content
			content.WriteString(delta)
			if onText != nil {
				onText(delta)
			}
		}

		// Tool calls arrive in the final chunk(s).
		for _, tc := range chunk.Message.ToolCalls {
			toolCalls = append(toolCalls, tc.toToolCall())
		}

		if chunk.Done {
			usage = Usage{
				PromptTokens:     chunk.PromptEvalCount,
				CompletionTokens: chunk.EvalCount,
				TotalTokens:      chunk.PromptEvalCount + chunk.EvalCount,
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Response{}, err
	}

	finalContent := strings.TrimSpace(content.String())
	// Fallback: Qwen XML-format tool calls in content (see Complete).
	if len(toolCalls) == 0 && strings.Contains(finalContent, qwenFuncStart) {
		if parsed := parseQwenXMLToolCalls(finalContent); len(parsed) > 0 {
			toolCalls = parsed
		}
	}

	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}

	return Response{
		Content:      finalContent,
		ToolCalls:    toolCalls,
		FinishReason: finish,
		Usage:        usage,
	}, nil
}
