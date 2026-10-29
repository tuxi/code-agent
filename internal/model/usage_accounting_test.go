package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type accountingProvider struct {
	responses []Response
	errs      []error
	calls     int
	stream    Response
	streamErr error
	after     func()
}

func (p *accountingProvider) Complete(context.Context, Request) (Response, error) {
	i := p.calls
	p.calls++
	if p.after != nil {
		p.after()
	}
	return p.responses[i], p.errs[i]
}
func (p *accountingProvider) CompleteStream(context.Context, Request, func(string), func(string)) (Response, error) {
	if p.after != nil {
		p.after()
	}
	return p.stream, p.streamErr
}

func countedResponse(prompt, output int) Response {
	return Response{ActualModel: "served", Content: "ok", Usage: Usage{PromptTokens: prompt, CompletionTokens: output, TotalTokens: prompt + output, CachedPromptTokens: prompt / 2, BillingUnits: int64(output)}}
}

func TestUsageAccountingAcrossRetriesAndFallbacks(t *testing.T) {
	bad := countedResponse(100, 20)
	bad.ToolCalls = toolCallResponse(`{"path":`).ToolCalls
	bad.FinishReason = "length"
	good := countedResponse(150, 30)
	for _, tc := range []struct {
		name       string
		stream     bool
		responses  []Response
		errs       []error
		streamResp Response
		streamErr  error
		want       Usage
		lastPrompt int
		failed     bool
	}{
		{"retry succeeds", false, []Response{bad, good}, []error{nil, nil}, Response{}, nil, Usage{250, 50, 300, 50, 125}, 150, false},
		{"all malformed", false, []Response{bad, bad}, []error{nil, nil}, Response{}, nil, Usage{200, 40, 240, 40, 100}, 100, true},
		{"provider error with usage", false, []Response{bad}, []error{errors.New("permanent")}, Response{}, nil, bad.Usage, 100, true},
		{"stream validation then fallback", true, []Response{good}, []error{nil}, bad, nil, Usage{250, 50, 300, 50, 125}, 150, false},
		{"stream error then retries fail", true, []Response{bad, bad}, []error{nil, nil}, bad, io.ErrUnexpectedEOF, Usage{300, 60, 360, 60, 150}, 100, true},
		{"quota no fallback", true, nil, nil, bad, &APIError{StatusCode: 429, Code: "quota_exceeded"}, bad.Usage, 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &accountingProvider{responses: tc.responses, errs: tc.errs, stream: tc.streamResp, streamErr: tc.streamErr}
			observer := &capturingObserver{}
			p := &ResilientProvider{Inner: inner, MaxRetries: len(tc.responses) - 1, sleep: noSleep(), LogWriter: io.Discard, Observer: observer}
			var resp Response
			var err error
			if tc.stream {
				resp, err = p.CompleteStream(context.Background(), Request{}, nil, nil)
			} else {
				resp, err = p.Complete(context.Background(), Request{})
			}
			if (err != nil) != tc.failed || resp.AccountingUsage() != tc.want || resp.Usage.PromptTokens != tc.lastPrompt {
				t.Fatalf("response=%+v accounting=%+v error=%v", resp, resp.AccountingUsage(), err)
			}
			if tc.failed && (resp.Content != "" || resp.HasToolCalls() || resp.ReasoningContent != "" || len(resp.Raw) > 0) {
				t.Fatal("failed response exposes replayable output")
			}
			var prompt, output, cached, attempts int
			for _, stat := range observer.stats {
				prompt += stat.PromptTokens
				output += stat.CompletionTokens
				cached += stat.CachedPromptTokens
				attempts += stat.Attempts
				for _, attempt := range stat.Trace {
					if attempt.Usage.TotalTokens == 0 {
						t.Fatalf("lost per-attempt usage: %+v", stat)
					}
				}
			}
			wantAttempts := len(tc.responses)
			if tc.stream {
				wantAttempts++
			}
			if prompt != tc.want.PromptTokens || output != tc.want.CompletionTokens || cached != tc.want.CachedPromptTokens || attempts != wantAttempts {
				t.Fatalf("telemetry duplicated/lost usage: %+v", observer.stats)
			}
		})
	}
}

func TestUsageSurvivesCallerCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := countedResponse(20, 10)
			inner := &accountingProvider{responses: []Response{observed}, errs: []error{context.Canceled}, stream: observed, streamErr: context.Canceled, after: cancel}
			p := &ResilientProvider{Inner: inner, MaxRetries: 2, LogWriter: io.Discard}
			var resp Response
			var err error
			if stream {
				resp, err = p.CompleteStream(ctx, Request{}, nil, nil)
			} else {
				resp, err = p.Complete(ctx, Request{})
			}
			if !errors.Is(err, context.Canceled) || resp.AccountingUsage() != observed.Usage || resp.HasToolCalls() || resp.Content != "" {
				t.Fatalf("%+v %v", resp, err)
			}
		})
	}
}

func TestAdaptersRetainObservedUsageOnFailure(t *testing.T) {
	for _, wire := range []string{"chat", "responses"} {
		for _, mode := range []string{"body_error", "http_error", "stream_error", "stream_lost_terminal"} {
			t.Run(wire+"/"+mode, func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "http_error" {
						w.WriteHeader(http.StatusBadRequest)
					}
					chat := `{"model":"served","error":{"code":400,"message":"failed"},"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":40}}`
					responses := `{"model":"served","status":"failed","error":{"code":400,"message":"failed"},"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":40}}}`
					if mode == "body_error" || mode == "http_error" {
						if wire == "chat" {
							fmt.Fprint(w, chat)
						} else {
							fmt.Fprint(w, responses)
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if wire == "chat" {
						// Two cumulative snapshots must not double the charge.
						usage := `{"model":"served","usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":40}}`
						fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", usage, usage)
						if mode == "stream_error" {
							fmt.Fprintf(w, "data: %s\n\n", chat)
						}
					} else {
						if mode == "stream_error" {
							fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":%s}\n\n", responses)
						} else {
							fmt.Fprintf(w, "data: {\"type\":\"response.in_progress\",\"response\":%s}\n\n", responses)
						}
					}
				}))
				defer s.Close()
				var p Provider
				if wire == "chat" {
					p = NewOpenAICompatibleProviderWithKey(s.URL, "")
				} else {
					p = NewResponsesProviderWithKey(s.URL, "")
				}
				var resp Response
				var err error
				if mode == "stream_error" || mode == "stream_lost_terminal" {
					resp, err = p.(StreamingProvider).CompleteStream(context.Background(), Request{}, nil, nil)
				} else {
					resp, err = p.Complete(context.Background(), Request{})
				}
				if err == nil || resp.Usage.PromptTokens != 100 || resp.Usage.CompletionTokens != 20 || resp.Usage.CachedPromptTokens != 40 || resp.ActualModel != "served" || resp.Content != "" || resp.HasToolCalls() {
					t.Fatalf("%+v %v", resp, err)
				}
			})
		}
	}
}
