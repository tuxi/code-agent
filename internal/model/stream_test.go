package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompleteStreamParsesTextToolCallsAndUsage(t *testing.T) {
	sse := strings.Join([]string{
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

	p := NewOpenAICompatibleProvider(srv.URL, "key")
	var deltas []string
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(d string) {
		deltas = append(deltas, d)
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Fatalf("onText deltas = %q, want 'Hello world'", got)
	}
	if resp.Content != "Hello world" {
		t.Fatalf("accumulated content = %q", resp.Content)
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

// fakeStreamProvider implements both Complete and CompleteStream so the resilient
// wrapper's fallback can be exercised.
type fakeStreamProvider struct {
	streamErr    error
	completeResp Response
}

func (f *fakeStreamProvider) Complete(context.Context, Request) (Response, error) {
	return f.completeResp, nil
}
func (f *fakeStreamProvider) CompleteStream(_ context.Context, _ Request, onText func(string)) (Response, error) {
	if f.streamErr != nil {
		onText("partial") // a half-stream the renderer would discard on fallback
		return Response{}, f.streamErr
	}
	onText("streamed")
	return Response{Content: "streamed"}, nil
}

func TestResilientCompleteStreamFallsBackToComplete(t *testing.T) {
	inner := &fakeStreamProvider{streamErr: errors.New("boom"), completeResp: Response{Content: "fallback answer"}}
	p := &ResilientProvider{Inner: inner}
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(string) {})
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
	resp, err := p.CompleteStream(context.Background(), Request{Model: "m"}, func(d string) { got += d })
	if err != nil {
		t.Fatal(err)
	}
	if got != "streamed" || resp.Content != "streamed" {
		t.Fatalf("stream path: deltas=%q resp=%q", got, resp.Content)
	}
}
