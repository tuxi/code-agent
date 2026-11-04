package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// These characterize the frozen adapter, not live OpenCode or budget compliance.
// Replacing RoundTripper ensures no credential or request reaches a real API.
func TestOpenCodeReviewWireToolCallWithoutOutputOverride(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "duration_smoke/duration.py", "new": strings.Repeat("x", 8192)})
	call := ToolCall{ID: "offline-call", Type: "function", Function: FunctionCall{Name: "edit_file", Arguments: string(args)}}
	p := NewOpenAICompatibleProviderWithKey("https://opencode.ai/inference/go/openai/v1", "offline-review-placeholder")
	p.HTTPClient = &http.Client{Transport: modelRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != "https://opencode.ai/inference/go/openai/v1/chat/completions" {
			t.Fatal("wrong wire API", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-opencode-session") != "offline-session" {
			t.Fatal("lost stable provider session header")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"max_tokens", "max_output_tokens", "session_id", "execution_id"} {
			if _, exists := body[field]; exists {
				t.Fatal("unexpected caller override or gateway extension", field)
			}
		}
		if string(body["model"]) != `"qwen3.8-flash"` || string(body["reasoning_effort"]) != `"medium"` {
			t.Fatal("model configuration changed")
		}
		raw, _ := json.Marshal(map[string]any{"model": "qwen3.8-flash", "choices": []any{map[string]any{"message": map[string]any{"tool_calls": []ToolCall{call}}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	r, err := p.Complete(context.Background(), Request{SessionID: "offline-session", Model: "qwen3.8-flash", ReasoningEffort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Function.Arguments != string(args) {
		t.Fatal("tool arguments truncated or changed")
	}
}

func TestOpenCodeReviewBillingExtensionsAreNotTypedAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			usage := `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cost":0,"opencode":{"billing_source":"go","list_cost":0.001,"allowance":0.002,"tokens":{"input":100,"output":20,"reasoning":5,"cache_read":40,"cache_write_5m":0,"cache_write_1h":0}}}`
			body := `{"model":"qwen3.8-flash","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":` + usage + `}`
			if stream {
				body = "data: {\"model\":\"qwen3.8-flash\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":" + usage + "}\n\ndata: [DONE]\n\n"
			}
			p := NewOpenAICompatibleProviderWithKey("https://opencode.ai/inference/go/openai/v1", "offline-review-placeholder")
			p.HTTPClient = &http.Client{Transport: modelRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			var r Response
			var err error
			if stream {
				r, err = p.CompleteStream(context.Background(), Request{Model: "qwen3.8-flash"}, nil, nil)
			} else {
				r, err = p.Complete(context.Background(), Request{Model: "qwen3.8-flash"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Usage.PromptTokens != 100 || r.Usage.CompletionTokens != 20 {
				t.Fatal("lost standard usage")
			}
			encoded, _ := json.Marshal(r.AccountingUsage())
			if strings.Contains(string(encoded), `"cost"`) || strings.Contains(string(encoded), `"allowance"`) {
				t.Fatal("review needs updating: typed billing support changed")
			}
			if stream && len(r.Raw) != 0 {
				t.Fatal("review needs updating: stream raw retention changed")
			}
			if !stream && !strings.Contains(string(r.Raw), `"allowance"`) {
				t.Fatal("lost successful non-stream raw response")
			}
		})
	}
}

func TestOpenCodeReviewPolicy429MayConsumeMultiplePhysicalRequests(t *testing.T) {
	calls := 0
	p := NewOpenAICompatibleProviderWithKey("https://opencode.ai/inference/go/openai/v1", "offline-review-placeholder")
	p.HTTPClient = &http.Client{Transport: modelRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"policy_budget_exceeded","message":"Upstream request failed: Policy budget exceeded"}}`))}, nil
	})}
	r := &ResilientProvider{Inner: p, MaxRetries: 2, sleep: noSleep(), LogWriter: io.Discard}
	if _, err := r.CompleteStream(context.Background(), Request{Model: "qwen3.8-flash"}, nil, nil); err == nil {
		t.Fatal("budget failure became success")
	}
	// One streaming attempt followed by three non-stream attempts. This is a
	// NO-GO finding: a logical-call counter cannot enforce a physical budget.
	if calls != 4 {
		t.Fatalf("review needs updating: physical attempts=%d, expected frozen behavior of 4", calls)
	}
}
