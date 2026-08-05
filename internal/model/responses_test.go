package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code-agent/internal/credential"
)

// decodeRequestBody decodes the Responses request body a handler received.
func decodeRequestBody(t *testing.T, r *http.Request) responsesRequest {
	t.Helper()
	var body responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

func TestResponsesRequestBodyMapping(t *testing.T) {
	var got responsesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("request path = %q, want /responses", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("authorization = %q, want Bearer sk-test", auth)
		}
		got = decodeRequestBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "sk-test")
	_, err := p.Complete(context.Background(), Request{
		Model: "deepseek-v4-flash",
		Messages: []Message{
			{Role: RoleSystem, Content: "system one"},
			{Role: RoleSystem, Content: "system two"},
			{Role: RoleUser, Content: "hello"},
			{Role: RoleAssistant, Content: "let me check", ToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: FunctionCall{Name: "list_files", Arguments: `{"path":"."}`}},
			}},
			{Role: RoleTool, ToolCallID: "call_1", Content: "a.txt\nb.txt"},
		},
		Tools: []ToolDefinition{{
			Type:     "function",
			Function: ToolFunction{Name: "list_files", Description: "list", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Model != "deepseek-v4-flash" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Instructions != "system one\n\nsystem two" {
		t.Errorf("instructions = %q, want joined system messages", got.Instructions)
	}
	if len(got.Input) != 4 {
		t.Fatalf("input items = %d, want 4", len(got.Input))
	}
	// user → message(user, input_text)
	u := got.Input[0]
	if u.Type != "message" || u.Role != "user" || len(u.Content) != 1 || u.Content[0].Type != "input_text" || u.Content[0].Text != "hello" {
		t.Errorf("user item = %+v", u)
	}
	// assistant content → message(assistant, output_text)
	a := got.Input[1]
	if a.Type != "message" || a.Role != "assistant" || len(a.Content) != 1 || a.Content[0].Type != "output_text" || a.Content[0].Text != "let me check" {
		t.Errorf("assistant message item = %+v", a)
	}
	// assistant tool call → function_call item
	fc := got.Input[2]
	if fc.Type != "function_call" || fc.CallID != "call_1" || fc.Name != "list_files" || fc.Arguments != `{"path":"."}` {
		t.Errorf("function_call item = %+v", fc)
	}
	// tool result → function_call_output
	fo := got.Input[3]
	if fo.Type != "function_call_output" || fo.CallID != "call_1" || fo.Output != "a.txt\nb.txt" {
		t.Errorf("function_call_output item = %+v", fo)
	}
	// tools are FLATTENED to the Responses shape: name/description/parameters at
	// the top level of each tool object (DeepSeek rejects the Chat-Completions
	// nested {"function":{...}} form with "tools[0]: missing field name").
	if got.Tools == nil || len(*got.Tools) != 1 {
		t.Fatalf("tools = %+v", got.Tools)
	}
	tool := (*got.Tools)[0]
	if tool.Name != "list_files" || tool.Description != "list" || string(tool.Parameters) != `{"type":"object"}` {
		t.Errorf("flat tool = %+v", tool)
	}
	rawBody, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if strings.Contains(string(rawBody), `"function":{`) {
		t.Errorf("serialized tools still contain the Chat-Completions nested wrapper: %s", rawBody)
	}
	if !strings.Contains(string(rawBody), `"name":"list_files"`) {
		t.Errorf("serialized tools lack the top-level name: %s", rawBody)
	}
	if got.Stream {
		t.Errorf("non-streaming request must not set stream")
	}
}

func TestResponsesCompleteParsesMixedOutput(t *testing.T) {
	body := `{"id":"r_1","status":"completed","output":[
		{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"think hard"}]},
		{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"final answer"}]},
		{"type":"function_call","id":"fc_item","call_id":"call_42","name":"read_file","arguments":"{\"path\":\"x\"}"}
	],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":3,"total_tokens":13}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	resp, err := p.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "final answer" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.ReasoningContent != "think hard" {
		t.Errorf("reasoning = %q", resp.ReasoningContent)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_42" || tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"x"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 13 || resp.Usage.CachedPromptTokens != 4 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestResponsesToolCallFallsBackToItemID(t *testing.T) {
	body := `{"id":"r_1","status":"completed","output":[
		{"type":"function_call","id":"fc_item","name":"list_files","arguments":"{}"}
	],"usage":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	resp, err := p.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "fc_item" {
		t.Errorf("call id fallback failed: %+v", resp.ToolCalls)
	}
}

func TestResponsesCompleteStatusFailedReturnsRetryableError(t *testing.T) {
	body := `{"id":"r_1","status":"failed","output":[],"error":{"type":"upstream_error","code":"upstream","message":"boom"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	_, err := p.Complete(context.Background(), Request{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || apiErr.Type != "upstream_error" || apiErr.Code != "upstream" || apiErr.Message != "boom" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestResponsesCompleteStreamParsesDeltasAndUsage(t *testing.T) {
	// The terminal event must be a SINGLE SSE line (a multi-line JSON body would
	// be split across data: lines and the line-based parser would never see it —
	// real Responses streams emit one event per line). Build it via json.Marshal
	// so escaping stays correct.
	terminal, err := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "r_1",
			"status": "completed",
			"output": []map[string]any{
				{"type": "reasoning", "id": "rs_1", "content": []map[string]string{{"type": "reasoning_text", "text": "First reason"}}},
				{"type": "message", "id": "msg_1", "content": []map[string]string{{"type": "output_text", "text": "Hello world\n"}}},
				{"type": "function_call", "id": "fc_item", "call_id": "call_9", "name": "list_files", "arguments": `{"path":"x"}`},
			},
			"usage": map[string]any{
				"input_tokens":          10,
				"input_tokens_details":  map[string]int{"cached_tokens": 6},
				"output_tokens":         3,
				"output_tokens_details": map[string]int{"reasoning_tokens": 1},
				"total_tokens":          13,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal terminal event: %v", err)
	}
	sse := strings.Join([]string{
		`data: {"type":"response.reasoning_text.delta","delta":"First "}`,
		`data: {"type":"response.reasoning_text.delta","delta":"reason"}`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		`data: {"type":"response.output_text.delta","delta":" world"}`,
		`data: {"type":"response.output_text.delta","delta":"\n"}`,
		"data: " + string(terminal),
	}, "\n\n") + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	var deltas, reasoningDeltas []string
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(d string) {
		deltas = append(deltas, d)
	}, func(d string) { reasoningDeltas = append(reasoningDeltas, d) })
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "Hello world\n" {
		t.Errorf("onText deltas = %q", got)
	}
	if got := strings.Join(reasoningDeltas, ""); got != "First reason" {
		t.Errorf("onReasoning deltas = %q", got)
	}
	// Terminal event is authoritative: rebuilt from the embedded response object,
	// with whitespace trimmed by responsesFromOutput (mirroring the openai adapter).
	if resp.Content != "Hello world" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.ReasoningContent != "First reason" {
		t.Errorf("reasoning = %q", resp.ReasoningContent)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_9" || resp.ToolCalls[0].Function.Name != "list_files" {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CachedPromptTokens != 6 || resp.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestResponsesCompleteStreamToleratesDoneMarker(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		`data: [DONE]`,
		`data: {"type":"response.completed","response":{"id":"r_1","status":"completed","output":[{"type":"message","id":"m","content":[{"type":"output_text","text":"ok"}]}],"usage":{}}}`,
	}, "\n\n") + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, nil, nil)
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestResponsesCompleteStreamEndsBeforeTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	_, err := p.CompleteStream(context.Background(), Request{Model: "m"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "before a terminal event") {
		t.Fatalf("error = %v, want missing-terminal error", err)
	}
}

func TestResponsesCompleteStreamFailedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"id":"r_1","status":"failed","error":{"type":"upstream_error","code":"upstream","message":"stream boom"}}}` + "\n\n"))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	_, err := p.CompleteStream(context.Background(), Request{Model: "m"}, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || apiErr.Type != "upstream_error" || apiErr.Message != "stream boom" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestResponsesAuthErrorsIncludeTargetAndRedactBody(t *testing.T) {
	target := credential.Target{Namespace: "llm", Name: "company-production"}
	resolver := credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "super-secret-key"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"upstream","message":"echo super-secret-key"}}`))
	}))
	defer srv.Close()

	p := NewResponsesProvider(srv.URL, resolver, target)
	_, err := p.Complete(context.Background(), Request{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want APIError", err, err)
	}
	if apiErr.CredentialTarget != "llm/company-production" {
		t.Errorf("credential target = %q", apiErr.CredentialTarget)
	}
	if apiErr.Code != "auth_expired" || apiErr.Type != "authentication_error" {
		t.Errorf("classification = %q/%q", apiErr.Code, apiErr.Type)
	}
	if strings.Contains(err.Error(), "super-secret-key") || apiErr.Body != "" {
		t.Errorf("authentication error leaked provider body: %v", err)
	}
}

func TestResponsesResilientRetryOnTransientFailure(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"type":"server_error","code":"internal_error","message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_1","status":"completed","output":[{"type":"message","id":"m","content":[{"type":"output_text","text":"recovered"}]}],"usage":{}}`))
	}))
	defer srv.Close()

	inner := NewResponsesProviderWithKey(srv.URL, "key")
	// Bypass the defaultHTTPClient so the test server is reachable with retries.
	p := &ResilientProvider{Inner: inner, MaxRetries: 2, LogWriter: io.Discard}
	resp, err := p.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("resilient Complete: %v", err)
	}
	if resp.Content != "recovered" {
		t.Errorf("content = %q", resp.Content)
	}
	if calls != 2 {
		t.Errorf("attempts = %d, want 2 (one retry)", calls)
	}
}

func TestResponsesCompleteStreamFallsBackToComplete(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"))
			return // stream ends without a terminal event → error → fallback
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_1","status":"completed","output":[{"type":"message","id":"m","content":[{"type":"output_text","text":"fallback answer"}]}],"usage":{}}`))
	}))
	defer srv.Close()

	inner := NewResponsesProviderWithKey(srv.URL, "key")
	p := &ResilientProvider{Inner: inner, MaxRetries: 2, LogWriter: io.Discard}
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(string) {}, nil)
	if err != nil {
		t.Fatalf("failed stream should fall back, not error: %v", err)
	}
	if resp.Content != "fallback answer" {
		t.Errorf("should return the resilient Complete result, got %q", resp.Content)
	}
}

func TestResponsesEmptyMessagesAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_1","status":"completed","output":[],"usage":{}}`))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	resp, err := p.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "" || resp.FinishReason != "stop" || resp.Usage != (Usage{}) {
		t.Errorf("empty response = %+v", resp)
	}
}

func TestResponsesIncompleteStatusMapsToLength(t *testing.T) {
	body := `{"id":"r_1","status":"incomplete","output":[{"type":"message","id":"m","content":[{"type":"output_text","text":"cut off"}]}],"usage":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewResponsesProviderWithKey(srv.URL, "key")
	resp, err := p.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "cut off" || resp.FinishReason != "length" {
		t.Errorf("incomplete response = %+v", resp)
	}
}
