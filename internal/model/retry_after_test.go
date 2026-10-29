package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"30", 30 * time.Second, true},
		{" 60 ", time.Minute, true},
		{"0", 0, true},
		{now.Add(45 * time.Second).Format(http.TimeFormat), 45 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"", 0, false},
		{"-30", 0, false},
		{"1.5", 0, false},
		{"later", 0, false},
		{"9223372036854775807", 0, false},
		{"18446744073709551616", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, valid := parseRetryAfter(tc.value, now)
			if got != tc.want || valid != tc.valid {
				t.Fatalf("parseRetryAfter = (%s, %v), want (%s, %v)", got, valid, tc.want, tc.valid)
			}
		})
	}
}

func TestRetryAfterMessageFallback(t *testing.T) {
	for _, tc := range []struct {
		message string
		header  string
		want    time.Duration
	}{
		{"Upstream request failed: [ttft_admission_refused] the cell is at capacity; retry shortly (after 30 s)", "", 30 * time.Second},
		{"Please retry after 2 seconds", "", 2 * time.Second},
		{"Retry in 500 ms", "", 500 * time.Millisecond},
		{"retry after 1.5 minutes", "", 90 * time.Second},
		{"retry after 30 s", "60", time.Minute},
		{"retry after 30 s", "0", 0},
		{"retry after 30 s", "invalid", 30 * time.Second},
		{"retry after -30 s", "", 0},
		{"retry after 999999999999999999999999 s", "", 0},
		{"try again; request 30 took 5 seconds", "", 0},
	} {
		t.Run(tc.message+"/"+tc.header, func(t *testing.T) {
			err := withRetryAfter(&APIError{StatusCode: 429, Message: tc.message}, http.Header{"Retry-After": {tc.header}})
			if got := providerRetryDelay(fmt.Errorf("wrapped: %w", err)); got != tc.want {
				t.Fatalf("retry delay = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestProvidersHonorRetryAfter(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, embedded := range []bool{false, true} {
				for _, header := range []string{"", "60", "invalid"} {
					t.Run(fmt.Sprintf("%s/stream=%v/embedded=%v/header=%s", protocol, stream, embedded, header), func(t *testing.T) {
						calls := 0
						waited := false
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls++
							if calls > 1 {
								if !waited {
									t.Error("request replayed before waiting")
								}
								io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
								return
							}
							w.Header().Set("Retry-After", header)
							payload := `{"error":{"type":"server_error","message":"Upstream request failed: [ttft_admission_refused] the cell is at capacity; retry shortly (after 30 s)"}}`
							if !embedded {
								w.WriteHeader(http.StatusTooManyRequests)
							} else if stream {
								w.Header().Set("Content-Type", "text/event-stream")
								if protocol == "responses" {
									payload = `{"type":"response.failed","response":` + payload + `}`
								}
								payload = "data: " + payload + "\n\n"
							}
							io.WriteString(w, payload)
						}))
						defer server.Close()
						var inner Provider = NewOpenAICompatibleProviderWithKey(server.URL, "")
						if protocol == "responses" {
							inner = NewResponsesProviderWithKey(server.URL, "")
						}
						want := 30 * time.Second
						if header == "60" {
							want = time.Minute
						}
						var notified []RetryInfo
						p := &ResilientProvider{
							Inner: inner, MaxRetries: 1, Backoff: 500 * time.Millisecond, MaxBackoff: 8 * time.Second,
							LogWriter: io.Discard,
							sleep: func(delay time.Duration) {
								if calls != 1 || delay != want {
									t.Errorf("sleep at call %d for %s, want call 1 for %s", calls, delay, want)
								}
								waited = true
							},
						}
						ctx := WithRetryNotifier(context.Background(), func(info RetryInfo) { notified = append(notified, info) })
						var resp Response
						var err error
						if stream {
							resp, err = p.CompleteStream(ctx, Request{Model: "test"}, nil, nil)
						} else {
							resp, err = p.Complete(ctx, Request{Model: "test"})
						}
						if err != nil || resp.Content != "ok" || calls != 2 || !waited {
							t.Fatalf("response=%+v error=%v calls=%d waited=%v", resp, err, calls, waited)
						}
						if len(notified) != 1 || notified[0].Delay != want || notified[0].Fallback != stream {
							t.Fatalf("unexpected retry notifications: %+v", notified)
						}
					})
				}
			}
		}
	}
}

func TestResilientKeepsLongerLocalBackoff(t *testing.T) {
	inner := &fakeInner{errs: []error{&APIError{StatusCode: 429, RetryAfter: time.Second}}, resp: Response{Content: "ok"}}
	p := &ResilientProvider{Inner: inner, MaxRetries: 1, Backoff: 8 * time.Second, LogWriter: io.Discard,
		sleep: func(delay time.Duration) {
			if delay < 4*time.Second || delay > 8*time.Second {
				t.Errorf("local backoff shortened to %s", delay)
			}
		},
	}
	if _, err := p.Complete(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
}

type retryAfterStreamProvider struct{ *fakeInner }

func (p retryAfterStreamProvider) CompleteStream(context.Context, Request, func(string), func(string)) (Response, error) {
	return Response{Usage: Usage{PromptTokens: 7}}, &APIError{StatusCode: 429, RetryAfter: 30 * time.Second}
}

func TestRetryAfterWaitCanBeCanceled(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			inner := &fakeInner{errs: []error{&APIError{StatusCode: 429, RetryAfter: 30 * time.Second}}}
			var provider Provider = inner
			if stream {
				provider = retryAfterStreamProvider{inner}
			}
			p := &ResilientProvider{Inner: provider, MaxRetries: 1, LogWriter: io.Discard}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			notified := make(chan struct{})
			ctx = WithRetryNotifier(ctx, func(RetryInfo) { close(notified) })
			done := make(chan error, 1)
			go func() {
				var resp Response
				var err error
				if stream {
					resp, err = p.CompleteStream(ctx, Request{}, nil, nil)
				} else {
					resp, err = p.Complete(ctx, Request{})
				}
				if stream && resp.AccountingUsage().PromptTokens != 7 {
					t.Error("canceled stream lost usage")
				}
				done <- err
			}()
			select {
			case <-notified:
			case <-time.After(time.Second):
				t.Fatal("retry wait was not announced")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("want canceled, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("retry wait ignored cancellation")
			}
			wantCalls := 1
			if stream {
				wantCalls = 0
			}
			if inner.calls != wantCalls {
				t.Fatalf("calls after cancellation = %d, want %d", inner.calls, wantCalls)
			}
		})
	}
}
