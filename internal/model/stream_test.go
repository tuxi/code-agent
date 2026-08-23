package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code-agent/internal/credential"
)

type modelRoundTripFunc func(*http.Request) (*http.Response, error)

func (f modelRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCompleteStreamParsesTextToolCallsAndUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"First "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"reason"}}]}`,
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"content":" world"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]}}]}`,
		`data: {"choices":[{"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_cache_hit_tokens":6}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
	var deltas []string
	var reasoningDeltas []string
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(d string) {
		deltas = append(deltas, d)
	}, func(d string) { reasoningDeltas = append(reasoningDeltas, d) })
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Fatalf("onText deltas = %q, want 'Hello world'", got)
	}
	if resp.Content != "Hello world" {
		t.Fatalf("accumulated content = %q", resp.Content)
	}
	if got := strings.Join(reasoningDeltas, ""); got != "First reason" {
		t.Fatalf("onReasoning deltas=%q, want 'First reason'", got)
	}
	if resp.ReasoningContent != "First reason" {
		t.Fatalf("accumulated reasoning=%q, want 'First reason'", resp.ReasoningContent)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"x"}` {
		t.Fatalf("accumulated tool call = %+v", tc)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 3 || resp.Usage.CachedPromptTokens != 6 {
		t.Fatalf("usage from final chunk = %+v", resp.Usage)
	}
}

func TestCompleteStreamParsesOpenRouterReasoningFields(t *testing.T) {
	// OpenRouter normalizes reasoning into `reasoning` (flat string) and
	// `reasoning_details` (structured array) instead of DeepSeek's
	// `reasoning_content`. A reasoning-only stream must surface as reasoning,
	// not as an empty response.
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning":"thinking "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"hard"},{"type":"reasoning.encrypted","data":"REDACTED"}]}}]}`,
		`data: {"choices":[{"delta":{"content":"answer"}}]}`,
		`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
	var reasoningDeltas []string
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, nil, func(d string) {
		reasoningDeltas = append(reasoningDeltas, d)
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if resp.Content != "answer" {
		t.Fatalf("content = %q, want answer", resp.Content)
	}
	if got := strings.Join(reasoningDeltas, ""); got != "thinking hard" {
		t.Fatalf("onReasoning deltas = %q, want 'thinking hard' (encrypted entries contribute nothing)", got)
	}
	if resp.ReasoningContent != "thinking hard" {
		t.Fatalf("accumulated reasoning = %q, want 'thinking hard'", resp.ReasoningContent)
	}
}

func TestCompleteParsesOpenRouterReasoningFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning":"only thought","reasoning_details":[{"type":"reasoning.summary","summary":"summarized"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
	resp, err := p.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ReasoningContent != "only thought" {
		t.Fatalf("reasoning = %q, want the flat `reasoning` field", resp.ReasoningContent)
	}

	// details-only shape
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_details":[{"type":"reasoning.text","text":"from details"}]},"finish_reason":"stop"}]}`))
	}))
	defer srv2.Close()
	p2 := NewOpenAICompatibleProviderWithKey(srv2.URL, "key")
	resp2, err := p2.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.ReasoningContent != "from details" {
		t.Fatalf("reasoning = %q, want the reasoning_details text", resp2.ReasoningContent)
	}
}

func TestOpenAICompatibleAuthErrorsIncludeTargetAndRedactBody(t *testing.T) {
	target := credential.Target{Namespace: "llm", Name: "company-production"}
	resolver := credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "super-secret-key"},
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			provider := NewOpenAICompatibleProvider("https://provider.test/v1", resolver, target)
			provider.HTTPClient = &http.Client{Transport: modelRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: status,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`{"error":{"type":"upstream","message":"echo super-secret-key"}}`,
					)),
				}, nil
			})}

			_, err := provider.Complete(context.Background(), Request{
				Model: "model",
				Messages: []Message{{
					Role: RoleUser, Content: "hello",
				}},
			})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %T %v, want APIError", err, err)
			}
			if apiErr.CredentialTarget != "llm/company-production" {
				t.Fatalf("credential target = %q", apiErr.CredentialTarget)
			}
			if apiErr.Code != "auth_expired" || apiErr.Type != "authentication_error" {
				t.Fatalf("classification = %q/%q", apiErr.Code, apiErr.Type)
			}
			if strings.Contains(err.Error(), "super-secret-key") || apiErr.Body != "" {
				t.Fatalf("authentication error leaked provider body: %v", err)
			}
		})
	}
}

func TestCompleteStreamReturnsGatewaySSEError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"upstream rejected tool call\",\"type\":\"upstream_error\",\"code\":\"upstream_error\"}}\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
	_, err := p.CompleteStream(context.Background(), Request{Model: "m"}, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || apiErr.Type != "upstream_error" || apiErr.Code != "upstream_error" || apiErr.Message != "upstream rejected tool call" {
		t.Fatalf("APIError = %+v", apiErr)
	}
}

func TestCompletePreservesGatewayQuotaErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"daily allowance exhausted","type":"quota_exceeded","code":"quota_exceeded"}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
	_, err := p.Complete(context.Background(), Request{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "quota_exceeded" || apiErr.Code != "quota_exceeded" {
		t.Fatalf("APIError = %+v", apiErr)
	}
}

func TestCompleteSeparatesReasoningFromContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"final","reasoning_content":"analysis"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
	resp, err := p.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "final" || resp.ReasoningContent != "analysis" {
		t.Fatalf("content=%q reasoning=%q", resp.Content, resp.ReasoningContent)
	}
}

func TestCompleteStreamRequiresDoneMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
	_, err := p.CompleteStream(context.Background(), Request{Model: "m"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "before [DONE]") {
		t.Fatalf("error = %v, want missing DONE error", err)
	}
}

// fakeStreamProvider implements both Complete and CompleteStream so the resilient
// wrapper's fallback can be exercised.
type fakeStreamProvider struct {
	streamErr     error
	completeResp  Response
	completeCalls int
}

func (f *fakeStreamProvider) Complete(context.Context, Request) (Response, error) {
	f.completeCalls++
	return f.completeResp, nil
}
func (f *fakeStreamProvider) CompleteStream(_ context.Context, _ Request, onText, onReasoning func(string)) (Response, error) {
	if f.streamErr != nil {
		if onText != nil {
			onText("partial") // a half-stream the renderer would discard on fallback
		}
		if onReasoning != nil {
			onReasoning("partial reasoning")
		}
		return Response{}, f.streamErr
	}
	if onText != nil {
		onText("streamed")
	}
	if onReasoning != nil {
		onReasoning("reasoned")
	}
	return Response{Content: "streamed"}, nil
}

func TestResilientCompleteStreamFallsBackToComplete(t *testing.T) {
	inner := &fakeStreamProvider{streamErr: errors.New("boom"), completeResp: Response{Content: "fallback answer"}}
	p := &ResilientProvider{Inner: inner}
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(string) {}, nil)
	if err != nil {
		t.Fatalf("a failed stream should fall back, not error: %v", err)
	}
	if resp.Content != "fallback answer" {
		t.Fatalf("should return the resilient Complete result, got %q", resp.Content)
	}
}

func TestResilientCompleteStreamHappyPath(t *testing.T) {
	p := &ResilientProvider{Inner: &fakeStreamProvider{completeResp: Response{Content: "unused"}}}
	var got string
	var gotReasoning string
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(d string) { got += d }, func(d string) { gotReasoning += d })
	if err != nil {
		t.Fatal(err)
	}
	if got != "streamed" || resp.Content != "streamed" {
		t.Fatalf("stream path: deltas=%q resp=%q", got, resp.Content)
	}
	if gotReasoning != "reasoned" {
		t.Fatalf("reasoning deltas=%q, want reasoned", gotReasoning)
	}
}

func TestResilientCompleteStreamDoesNotFallbackForGatewayQuota(t *testing.T) {
	inner := &fakeStreamProvider{streamErr: &APIError{
		StatusCode: http.StatusTooManyRequests,
		Code:       "quota_exceeded",
		Message:    "daily allowance exhausted",
	}}
	p := &ResilientProvider{Inner: inner, LogWriter: io.Discard}

	_, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(string) {}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "quota_exceeded" {
		t.Fatalf("want quota_exceeded APIError, got %v", err)
	}
	if inner.completeCalls != 0 {
		t.Fatalf("Complete calls = %d, want 0 (quota must not fall back)", inner.completeCalls)
	}
}

// emptyStreamProvider completes the SSE exchange cleanly but returns neither
// text nor tool calls — what OpenRouter's zero-delta streams look like here.
type emptyStreamProvider struct {
	completeResp Response
	streamCalls  int
}

func (e *emptyStreamProvider) Complete(context.Context, Request) (Response, error) {
	return e.completeResp, nil
}

func (e *emptyStreamProvider) CompleteStream(context.Context, Request, func(string), func(string)) (Response, error) {
	e.streamCalls++
	return Response{FinishReason: "stop"}, nil // clean [DONE], zero deltas
}

// A clean-but-empty stream must not surface as ErrEmptyAssistantResponse: it
// falls back to the non-streamed path, which returns real content.
func TestResilientEmptyStreamFallsBackToComplete(t *testing.T) {
	inner := &emptyStreamProvider{completeResp: Response{Content: "rescued"}}
	p := &ResilientProvider{Inner: inner}

	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(string) {}, func(string) {})
	if err != nil {
		t.Fatalf("empty stream should fall back to Complete, got %v", err)
	}
	if resp.Content != "rescued" {
		t.Fatalf("content = %q, want the fallback answer", resp.Content)
	}
	if inner.streamCalls != 1 {
		t.Fatalf("stream calls = %d, want 1", inner.streamCalls)
	}
}

// A transport-successful empty response from Complete is retried like any other
// transient failure instead of failing the turn.
func TestResilientRetriesEmptyAssistantResponse(t *testing.T) {
	inner := &fakeInner{
		errs: []error{ErrEmptyAssistantResponse},
		resp: Response{Content: "second attempt worked"},
	}
	p := &ResilientProvider{Inner: inner, MaxRetries: 2, sleep: noSleep(), LogWriter: io.Discard}

	resp, err := p.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("empty response should be retried, got %v", err)
	}
	if resp.Content != "second attempt worked" {
		t.Fatalf("content = %q", resp.Content)
	}
	if inner.calls != 2 {
		t.Fatalf("calls = %d, want 2 (empty, then success)", inner.calls)
	}
}

// When every attempt comes back empty, the caller sees ErrEmptyAssistantResponse
// (wrapped by the exhausted-retries envelope).
func TestResilientExhaustsRetriesOnEmptyResponses(t *testing.T) {
	inner := &fakeInner{errs: []error{ErrEmptyAssistantResponse, ErrEmptyAssistantResponse}}
	p := &ResilientProvider{Inner: inner, MaxRetries: 1, sleep: noSleep(), LogWriter: io.Discard}

	_, err := p.Complete(context.Background(), Request{Model: "m"})
	if !errors.Is(err, ErrEmptyAssistantResponse) {
		t.Fatalf("err = %v, want ErrEmptyAssistantResponse", err)
	}
	if inner.calls != 2 {
		t.Fatalf("calls = %d, want 2", inner.calls)
	}
}
