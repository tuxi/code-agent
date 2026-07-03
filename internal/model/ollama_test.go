package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNewOllamaProviderDefaults(t *testing.T) {
	p := NewOllamaProvider("")
	if p.BaseURL != "http://localhost:11434" {
		t.Errorf("BaseURL = %q, want http://localhost:11434", p.BaseURL)
	}
	if p.HTTPClient == nil {
		t.Fatal("HTTPClient is nil")
	}
}

func TestNewOllamaProviderCustomURL(t *testing.T) {
	p := NewOllamaProvider("http://192.168.1.100:11434/")
	if p.BaseURL != "http://192.168.1.100:11434" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", p.BaseURL)
	}
}

// ── Tool call argument normalisation ────────────────────────────────────

func TestOllamaToolCallToToolCall_JSONObject(t *testing.T) {
	// Ollama native returns arguments as a JSON object.
	raw := json.RawMessage(`{"path":"."}`)
	tc := ollamaToolCall{ID: "call_123", Type: "function"}
	tc.Function.Name = "list_files"
	tc.Function.Arguments = raw

	out := tc.toToolCall()
	if out.ID != "call_123" {
		t.Errorf("ID = %q, want call_123", out.ID)
	}
	if out.Function.Name != "list_files" {
		t.Errorf("Name = %q, want list_files", out.Function.Name)
	}
	// Arguments should be the JSON object as a string.
	if out.Function.Arguments != `{"path":"."}` {
		t.Errorf("Arguments = %q, want JSON object string", out.Function.Arguments)
	}
}

func TestOllamaToolCallToToolCall_JSONString(t *testing.T) {
	// Some models emit arguments as a doubly-encoded JSON string.
	raw := json.RawMessage(`"{\"path\":\".\"}"`)
	tc := ollamaToolCall{}
	tc.Function.Name = "grep"
	tc.Function.Arguments = raw

	out := tc.toToolCall()
	if out.Function.Name != "grep" {
		t.Errorf("Name = %q, want grep", out.Function.Name)
	}
	if out.Function.Arguments != `{"path":"."}` {
		t.Errorf("Arguments = %q, want unquoted JSON string", out.Function.Arguments)
	}
}

func TestOllamaToolCallToToolCall_Empty(t *testing.T) {
	tc := ollamaToolCall{}
	tc.Function.Name = "no_args"
	out := tc.toToolCall()
	if out.Function.Arguments != "" {
		t.Errorf("Arguments = %q, want empty", out.Function.Arguments)
	}
}

// ── Complete (non-streaming) ────────────────────────────────────────────

func TestOllamaComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q, want /api/chat", r.URL.Path)
		}
		var body ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Stream {
			t.Error("Stream should be false for non-streaming call")
		}
		if body.KeepAlive != "5m" {
			t.Errorf("KeepAlive = %q, want 5m", body.KeepAlive)
		}
		// Return a response with tool calls (arguments as JSON object).
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ollamaChatResponse{
			Model: "test-model",
			Message: struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
			}{
				Role:    "assistant",
				Content: "I'll check that directory.",
				ToolCalls: []ollamaToolCall{
					{Function: struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					}{Name: "list_files", Arguments: json.RawMessage(`{"path":"."}`)}},
				},
			},
			Done:            true,
			PromptEvalCount: 42,
			EvalCount:       7,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL)
	resp, err := p.Complete(context.Background(), Request{
		Model:       "test-model",
		Temperature: 0.2,
		Messages:    []Message{{Role: RoleUser, Content: "list files"}},
		Tools:       []ToolDefinition{{Type: "function", Function: ToolFunction{Name: "list_files"}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "I'll check that directory." {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "list_files" {
		t.Errorf("tool name = %q", resp.ToolCalls[0].Function.Name)
	}
	if resp.ToolCalls[0].Function.Arguments != `{"path":"."}` {
		t.Errorf("tool args = %q", resp.ToolCalls[0].Function.Arguments)
	}
	if resp.Usage.PromptTokens != 42 || resp.Usage.CompletionTokens != 7 {
		t.Errorf("Usage = %+v, want 42/7", resp.Usage)
	}
}

// ── toOllamaMessages conversion ────────────────────────────────────────

func TestToOllamaMessages_ToolCallArguments(t *testing.T) {
	// ToolCall.Arguments is a JSON string (OpenAI wire format).
	// toOllamaMessages must convert it to a JSON value so Ollama's
	// native /api/chat receives a nested object, not a doubly-encoded string.
	msgs := []Message{
		{Role: RoleUser, Content: "hi"},
		{
			Role:    RoleAssistant,
			Content: "",
			ToolCalls: []ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: FunctionCall{
						Name:      "load_skill",
						Arguments: `{"name":"my-skill"}`,
					},
				},
			},
		},
	}
	converted := toOllamaMessages(msgs)
	if len(converted) != 2 {
		t.Fatalf("len = %d, want 2", len(converted))
	}
	// User message: no tool calls.
	if len(converted[0].ToolCalls) != 0 {
		t.Error("user message should have no tool calls")
	}
	// Assistant message: one tool call with arguments as a JSON value.
	if len(converted[1].ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(converted[1].ToolCalls))
	}
	tc := converted[1].ToolCalls[0]
	if tc.Function.Name != "load_skill" {
		t.Errorf("name = %q", tc.Function.Name)
	}
	if tc.ID != "call_1" {
		t.Errorf("id = %q", tc.ID)
	}
	// Arguments should be a JSON object (not a string).
	if string(tc.Function.Arguments) != `{"name":"my-skill"}` {
		t.Errorf("arguments = %s, want JSON object", tc.Function.Arguments)
	}
}

// ── Complete round-trip with tool calls in request ──────────────────────

func TestOllamaComplete_EchoesToolCallArgs(t *testing.T) {
	// When the model previously returned a tool call, the agent loop echoes
	// it back in the next request. Verify Ollama receives arguments as a
	// JSON object, not a double-encoded string.
	var capturedBody ollamaChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
			}{Role: "assistant", Content: "done"},
			Done:            true,
			PromptEvalCount: 1,
			EvalCount:       1,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL)
	_, err := p.Complete(context.Background(), Request{
		Model: "m",
		Messages: []Message{
			{Role: RoleUser, Content: "load skill x"},
			{
				Role:    RoleAssistant,
				Content: "",
				ToolCalls: []ToolCall{
					{
						ID:   "call_x",
						Type: "function",
						Function: FunctionCall{
							Name:      "load_skill",
							Arguments: `{"name":"x"}`,
						},
					},
				},
			},
			{Role: RoleTool, Content: "ok", ToolCallID: "call_x"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if len(capturedBody.Messages) != 3 {
		t.Fatalf("captured %d messages, want 3", len(capturedBody.Messages))
	}
	assistantMsg := capturedBody.Messages[1]
	if len(assistantMsg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(assistantMsg.ToolCalls))
	}
	args := string(assistantMsg.ToolCalls[0].Function.Arguments)
	if args != `{"name":"x"}` {
		t.Errorf("arguments = %s, want JSON object {\"name\":\"x\"}", args)
	}
}

func TestOllamaComplete_TextOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
			}{Role: "assistant", Content: "Hello!"},
			Done:            true,
			PromptEvalCount: 5,
			EvalCount:       2,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL)
	resp, err := p.Complete(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "Hello!" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
}

func TestOllamaComplete_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL)
	_, err := p.Complete(context.Background(), Request{Model: "bad-model", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error for 500 status")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
}

// ── CompleteStream ──────────────────────────────────────────────────────

func TestOllamaCompleteStream(t *testing.T) {
	// Simulate Ollama's NDJSON stream: each chunk carries a content DELTA
	// (the next token/word), NOT accumulated text. Tool calls and usage
	// arrive in the final chunk.
	chunks := []string{
		`{"model":"m","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":"I"},"done":false}`,
		`{"model":"m","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":" will"},"done":false}`,
		`{"model":"m","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":" check."},"done":false}`,
		`{"model":"m","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"list_files","arguments":{"path":"."}}}]},"done":true,"prompt_eval_count":10,"eval_count":3}`,
	}
	body := strings.Join(chunks, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Error("Stream should be true")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL)

	var deltas []string
	var mu sync.Mutex
	onText := func(s string) {
		mu.Lock()
		deltas = append(deltas, s)
		mu.Unlock()
	}

	resp, err := p.CompleteStream(context.Background(), Request{
		Model:       "m",
		Temperature: 0.2,
		Messages:    []Message{{Role: RoleUser, Content: "list files"}},
		Tools:       []ToolDefinition{{Type: "function", Function: ToolFunction{Name: "list_files"}}},
	}, onText)
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}

	// Verify deltas were passed through directly (real Ollama sends deltas).
	if strings.Join(deltas, "") != "I will check." {
		t.Errorf("deltas = %q, want 'I will check.'", strings.Join(deltas, ""))
	}
	if len(deltas) != 3 {
		t.Errorf("expected 3 deltas (I /  will /  check.), got %d: %v", len(deltas), deltas)
	}

	// Verify final response.
	if resp.Content != "I will check." {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 3 {
		t.Errorf("Usage = %+v, want 10/3", resp.Usage)
	}
}

// ── Qwen JSON + XML tool-call fallback ─────────────────────────────────

func TestParseQwenJSONToolCalls(t *testing.T) {
	content := `{"name":"read_file","arguments":{"path":"main.go"}}`
	calls := parseQwenJSONToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	tc := calls[0]
	if tc.Function.Name != "read_file" {
		t.Errorf("name = %q, want read_file", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("args = %s", tc.Function.Arguments)
	}
}

func TestParseQwenJSONToolCalls_PlainText(t *testing.T) {
	if calls := parseQwenJSONToolCalls("hello world"); calls != nil {
		t.Errorf("expected nil for plain text, got %v", calls)
	}
	if calls := parseQwenJSONToolCalls(""); calls != nil {
		t.Errorf("expected nil for empty, got %v", calls)
	}
}

func TestOllamaComplete_QwenJSONFallback(t *testing.T) {
	// Simulate qwen2.5-coder returning JSON in content with no tool_calls.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
			}{
				Role:    "assistant",
				Content: `{"name":"read_file","arguments":{"path":"main.go"}}`,
			},
			Done:            true,
			PromptEvalCount: 5,
			EvalCount:       3,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL)
	resp, err := p.Complete(context.Background(), Request{
		Model:    "qwen2.5-coder:14b",
		Messages: []Message{{Role: RoleUser, Content: "read main.go"}},
		Tools:    []ToolDefinition{{Type: "function", Function: ToolFunction{Name: "read_file"}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("name = %q", resp.ToolCalls[0].Function.Name)
	}
}

func TestParseQwenXMLToolCalls_SingleParam(t *testing.T) {
	content := `<function=plan_workflow>
<parameter=goal>
生成一张橘猫在夕阳背景下玩耍的图片
</parameter>
</function>`

	calls := parseQwenXMLToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	tc := calls[0]
	if tc.Function.Name != "plan_workflow" {
		t.Errorf("name = %q, want plan_workflow", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"goal":"生成一张橘猫在夕阳背景下玩耍的图片"}` {
		t.Errorf("args = %s", tc.Function.Arguments)
	}
	if tc.ID != "plan_workflow" {
		t.Errorf("id = %q", tc.ID)
	}
	if tc.Type != "function" {
		t.Errorf("type = %q", tc.Type)
	}
}

func TestParseQwenXMLToolCalls_MultiParam(t *testing.T) {
	content := `<function=search>
<parameter=query>
golang error handling
</parameter>
<parameter=max_results>
10
</parameter>
</function>`

	calls := parseQwenXMLToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	tc := calls[0]
	if tc.Function.Name != "search" {
		t.Errorf("name = %q", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"max_results":"10","query":"golang error handling"}` {
		t.Errorf("args = %s", tc.Function.Arguments)
	}
}

func TestParseQwenXMLToolCalls_PlainTextPassthrough(t *testing.T) {
	// Content without <function= should return nil (no tool calls).
	calls := parseQwenXMLToolCalls("这是一段普通的文本回复")
	if calls != nil {
		t.Errorf("expected nil for plain text, got %v", calls)
	}

	calls = parseQwenXMLToolCalls("")
	if calls != nil {
		t.Errorf("expected nil for empty string, got %v", calls)
	}
}

func TestParseQwenXMLToolCalls_MultipleFunctions(t *testing.T) {
	content := `<function=read_file>
<parameter=path>
main.go
</parameter>
</function>
<function=write_file>
<parameter=path>
output.txt
</parameter>
<parameter=content>
hello
</parameter>
</function>`

	calls := parseQwenXMLToolCalls(content)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("first call name = %q", calls[0].Function.Name)
	}
	if calls[1].Function.Name != "write_file" {
		t.Errorf("second call name = %q", calls[1].Function.Name)
	}
}

func TestOllamaComplete_QwenXMLFallback(t *testing.T) {
	// Simulate qwen3-coder returning XML-format function calls in content
	// with no native tool_calls and a bare {{ .Prompt }} template.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
			}{
				Role:    "assistant",
				Content: "<function=plan_workflow>\n<parameter=goal>\ntest goal\n</parameter>\n</function>",
			},
			Done:            true,
			PromptEvalCount: 10,
			EvalCount:       5,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider(srv.URL)
	resp, err := p.Complete(context.Background(), Request{
		Model:    "qwen3-coder",
		Messages: []Message{{Role: RoleUser, Content: "plan a workflow"}},
		Tools:    []ToolDefinition{{Type: "function", Function: ToolFunction{Name: "plan_workflow"}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1 (parsed from XML content)", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "plan_workflow" {
		t.Errorf("name = %q", resp.ToolCalls[0].Function.Name)
	}
}
