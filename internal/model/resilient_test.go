package model

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// fakeInner is a scripted inner provider. It returns errs[i] on attempt i (nil =
// success returning resp), and records the message count each attempt saw so a
// test can prove retries do not mutate or duplicate the request.
type fakeInner struct {
	errs     []error
	resp     Response
	calls    int
	seenLens []int
	block    bool // block until the attempt context is canceled (timeout test)
}

func (f *fakeInner) Complete(ctx context.Context, req Request) (Response, error) {
	i := f.calls
	f.calls++
	f.seenLens = append(f.seenLens, len(req.Messages))
	if f.block {
		<-ctx.Done()
		return Response{}, ctx.Err()
	}
	if i < len(f.errs) && f.errs[i] != nil {
		return Response{}, f.errs[i]
	}
	return f.resp, nil
}

// timeoutErr is a net.Error reporting a timeout, like a transport-level read
// timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func noSleep() func(time.Duration) { return func(time.Duration) {} }

func TestResilientRetriesThenSucceeds(t *testing.T) {
	inner := &fakeInner{
		errs: []error{timeoutErr{}, &APIError{StatusCode: 503}},
		resp: Response{Content: "ok"},
	}
	p := &ResilientProvider{Inner: inner, MaxRetries: 3, sleep: noSleep(), LogWriter: io.Discard}

	resp, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
	if inner.calls != 3 {
		t.Fatalf("calls = %d, want 3 (timeout, 503, success)", inner.calls)
	}
}

func TestResilientNonRetryableStopsImmediately(t *testing.T) {
	inner := &fakeInner{errs: []error{&APIError{StatusCode: 400, Message: "bad request"}}}
	p := &ResilientProvider{Inner: inner, MaxRetries: 3, sleep: noSleep(), LogWriter: io.Discard}

	_, err := p.Complete(context.Background(), Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("want a 400 APIError, got %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("calls = %d, want 1 (4xx must not retry)", inner.calls)
	}
}

func TestResilientExhaustsRetries(t *testing.T) {
	inner := &fakeInner{errs: []error{timeoutErr{}, timeoutErr{}, timeoutErr{}}}
	p := &ResilientProvider{Inner: inner, MaxRetries: 2, sleep: noSleep(), LogWriter: io.Discard}

	_, err := p.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if inner.calls != 3 {
		t.Fatalf("calls = %d, want 3 (1 + 2 retries)", inner.calls)
	}
	var ne net.Error
	if !errors.As(err, &ne) {
		t.Fatalf("exhausted error should wrap the last transport error, got %v", err)
	}
}

func TestResilientStopsOnCallerCancel(t *testing.T) {
	inner := &fakeInner{resp: Response{Content: "ok"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &ResilientProvider{Inner: inner, MaxRetries: 3, sleep: noSleep(), LogWriter: io.Discard}

	_, err := p.Complete(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if inner.calls != 0 {
		t.Fatalf("calls = %d, want 0 (canceled before the first attempt)", inner.calls)
	}
}

// The #7 invariant: replaying across attempts must not append to or otherwise
// mutate the request's messages.
func TestResilientReplayDoesNotMutateMessages(t *testing.T) {
	inner := &fakeInner{errs: []error{timeoutErr{}, timeoutErr{}}, resp: Response{Content: "ok"}}
	p := &ResilientProvider{Inner: inner, MaxRetries: 5, sleep: noSleep(), LogWriter: io.Discard}

	msgs := []Message{{Role: RoleSystem, Content: "s"}, {Role: RoleUser, Content: "u"}}
	if _, err := p.Complete(context.Background(), Request{Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	for i, n := range inner.seenLens {
		if n != 2 {
			t.Fatalf("attempt %d saw %d messages, want 2 (retry must not append)", i, n)
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("caller's message slice was mutated: len = %d", len(msgs))
	}
}

func TestResilientPerAttemptTimeout(t *testing.T) {
	inner := &fakeInner{block: true} // never returns until its attempt context expires
	p := &ResilientProvider{Inner: inner, MaxRetries: 1, Timeout: 20 * time.Millisecond, sleep: noSleep(), LogWriter: io.Discard}

	start := time.Now()
	_, err := p.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if inner.calls != 2 {
		t.Fatalf("calls = %d, want 2 (each attempt times out and retries once)", inner.calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v; per-attempt timeout was not enforced", elapsed)
	}
}

func TestIsRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429", &APIError{StatusCode: 429}, true},
		{"500", &APIError{StatusCode: 500}, true},
		{"502", &APIError{StatusCode: 502}, true},
		{"503", &APIError{StatusCode: 503}, true},
		{"504", &APIError{StatusCode: 504}, true},
		{"408", &APIError{StatusCode: 408}, true},
		{"400", &APIError{StatusCode: 400}, false},
		{"401", &APIError{StatusCode: 401}, false},
		{"404", &APIError{StatusCode: 404}, false},
		{"422", &APIError{StatusCode: 422}, false},
		{"attempt-timeout", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"net-timeout", timeoutErr{}, true},
		{"decode-error", errors.New("decode response: bad json"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isRetryable(c.err); got != c.want {
			t.Errorf("isRetryable(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	p := &ResilientProvider{Backoff: 100 * time.Millisecond, MaxBackoff: 400 * time.Millisecond}
	// Full jitter keeps each delay in [d/2, d] where d doubles per attempt up to
	// the cap: 100, 200, 400, 400, ...
	bounds := []time.Duration{100, 200, 400, 400}
	for attempt, d := range bounds {
		got := p.backoffFor(attempt + 1)
		upper := d * time.Millisecond
		if got < upper/2 || got > upper {
			t.Errorf("backoffFor(%d) = %v, want within [%v, %v]", attempt+1, got, upper/2, upper)
		}
	}
}

type capturingObserver struct{ stats []RequestStat }

func (c *capturingObserver) Observe(s RequestStat) { c.stats = append(c.stats, s) }

// A retried-then-succeeded call emits one stat carrying the attempt/retry count
// and the successful response's prompt tokens.
func TestResilientEmitsSuccessStat(t *testing.T) {
	obs := &capturingObserver{}
	inner := &fakeInner{
		errs: []error{&APIError{StatusCode: 503}},
		resp: Response{Content: "ok", Usage: Usage{PromptTokens: 1234}},
	}
	p := &ResilientProvider{Inner: inner, MaxRetries: 2, sleep: noSleep(), LogWriter: io.Discard, Observer: obs}

	if _, err := p.Complete(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if len(obs.stats) != 1 {
		t.Fatalf("want 1 stat, got %d", len(obs.stats))
	}
	s := obs.stats[0]
	if !s.Success || s.Attempts != 2 || s.Retries != 1 || s.Model != "m" || s.PromptTokens != 1234 || s.ErrorClass != "" {
		t.Fatalf("unexpected stat: %+v", s)
	}
}

func TestResilientEmitsFailureStat(t *testing.T) {
	obs := &capturingObserver{}
	inner := &fakeInner{errs: []error{&APIError{StatusCode: 400}}}
	p := &ResilientProvider{Inner: inner, MaxRetries: 3, sleep: noSleep(), LogWriter: io.Discard, Observer: obs}

	if _, err := p.Complete(context.Background(), Request{Model: "m"}); err == nil {
		t.Fatal("expected an error")
	}
	if len(obs.stats) != 1 {
		t.Fatalf("want 1 stat, got %d", len(obs.stats))
	}
	s := obs.stats[0]
	if s.Success || s.Attempts != 1 || s.Retries != 0 || s.ErrorClass != "4xx" {
		t.Fatalf("unexpected stat: %+v", s)
	}
}

func TestResilientStatRecordsTimeout(t *testing.T) {
	obs := &capturingObserver{}
	inner := &fakeInner{block: true}
	p := &ResilientProvider{Inner: inner, MaxRetries: 1, Timeout: 20 * time.Millisecond, sleep: noSleep(), LogWriter: io.Discard, Observer: obs}

	_, _ = p.Complete(context.Background(), Request{Model: "m"})
	if len(obs.stats) != 1 {
		t.Fatalf("want 1 stat, got %d", len(obs.stats))
	}
	s := obs.stats[0]
	if !s.TimedOut || s.ErrorClass != "timeout" || s.Attempts != 2 {
		t.Fatalf("unexpected stat: %+v", s)
	}
}
