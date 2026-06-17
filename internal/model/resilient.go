package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"time"
)

// ResilientProvider wraps a Provider with transport-level resilience: a
// per-attempt timeout, bounded retries with exponential backoff + jitter, and
// error classification so only transient failures are retried.
//
// It is deliberately a PURE transport layer — it knows nothing about sessions or
// compaction. Replaying a request across attempts is SAFE: the inner provider
// only reads req.Messages to marshal the body, it never appends, so a retry can
// never duplicate tool/assistant messages or corrupt history. "Context too
// large" and other non-retryable 4xx are surfaced unchanged to the loop, which
// owns the decision to compact and try again.
type ResilientProvider struct {
	Inner Provider

	// MaxRetries is the number of retries AFTER the first attempt, so the total
	// number of attempts is MaxRetries + 1.
	MaxRetries int
	// Timeout bounds each individual attempt. Zero disables the per-attempt
	// deadline (the caller's context still applies).
	Timeout time.Duration
	// Backoff is the base delay before the first retry; retry n waits roughly
	// Backoff * 2^(n-1), jittered and capped at MaxBackoff.
	Backoff    time.Duration
	MaxBackoff time.Duration

	// Observer, if set, receives one RequestStat per Complete call (across all of
	// its attempts) for transport telemetry.
	Observer Observer
	// LogWriter receives one-line retry notices so a slow/failing request is not a
	// black box. Defaults to os.Stderr; set to io.Discard to silence.
	LogWriter io.Writer

	// sleep is injectable for tests; nil uses an interruptible real sleep.
	sleep func(time.Duration)
}

func (p *ResilientProvider) Complete(ctx context.Context, req Request) (resp Response, err error) {
	if p.Inner == nil {
		return Response{}, errors.New("resilient provider: nil inner provider")
	}
	attempts := p.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	// One RequestStat per call, finalized on exit (named returns let the defer see
	// the outcome regardless of which return path fired).
	start := time.Now()
	stat := RequestStat{At: start, Model: req.Model}
	defer func() {
		if p.Observer == nil {
			return
		}
		stat.Latency = time.Since(start)
		if stat.Retries = stat.Attempts - 1; stat.Retries < 0 {
			stat.Retries = 0
		}
		stat.Success = err == nil
		if err == nil {
			stat.PromptTokens = resp.Usage.PromptTokens
		} else {
			stat.ErrorClass = errorClass(err)
		}
		p.Observer.Observe(stat)
	}()

	for attempt := 1; attempt <= attempts; attempt++ {
		// Stop spending attempts the moment the caller no longer wants the work.
		if cerr := ctx.Err(); cerr != nil {
			return Response{}, cerr
		}
		stat.Attempts = attempt

		attemptCtx := ctx
		var cancel context.CancelFunc
		if p.Timeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, p.Timeout)
		}
		attemptStart := time.Now()
		resp, err = p.Inner.Complete(attemptCtx, req)
		attemptDur := time.Since(attemptStart)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return resp, nil
		}

		class := errorClass(err)
		if class == "timeout" {
			stat.TimedOut = true
		}

		// Caller cancellation/expiry (as opposed to our per-attempt timeout) is
		// terminal — checked before classification so a base-context deadline is
		// never mistaken for a retryable attempt timeout.
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if !isRetryable(err) {
			return Response{}, err
		}
		if attempt == attempts {
			break
		}
		delay := p.backoffFor(attempt)
		p.logf("[provider] attempt %d failed: %s after %s — retrying in %s\n",
			attempt, class, attemptDur.Round(100*time.Millisecond), delay.Round(10*time.Millisecond))
		if !p.wait(ctx, delay) {
			return Response{}, ctx.Err()
		}
	}
	return Response{}, fmt.Errorf("model call failed after %d attempt(s): %w", attempts, err)
}

// logf writes a one-line transport notice. Defaults to stderr.
func (p *ResilientProvider) logf(format string, args ...any) {
	w := p.LogWriter
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format, args...)
}

// backoffFor returns the (jittered) delay before the given retry attempt
// (1-based). Full jitter — a uniform pick in [d/2, d] — avoids synchronized
// retry storms across concurrent runs.
func (p *ResilientProvider) backoffFor(attempt int) time.Duration {
	if p.Backoff <= 0 {
		return 0
	}
	d := p.Backoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d <= 0 { // overflow
			d = p.MaxBackoff
			break
		}
		if p.MaxBackoff > 0 && d >= p.MaxBackoff {
			d = p.MaxBackoff
			break
		}
	}
	if p.MaxBackoff > 0 && (d <= 0 || d > p.MaxBackoff) {
		d = p.MaxBackoff
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// wait sleeps for d, returning false if the caller's context is canceled first.
func (p *ResilientProvider) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	if p.sleep != nil {
		p.sleep(d)
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isRetryable classifies a Complete error. Transient transport failures and
// 408/429/5xx are retryable; 4xx (bad request, auth, context-too-large),
// caller cancellation, and malformed responses are not.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Caller cancellation is never retryable. A bare DeadlineExceeded here is a
	// per-attempt timeout (the base-context case is handled in Complete before
	// this is reached), so it IS retryable.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusRequestTimeout, // 408
			http.StatusTooManyRequests,     // 429
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout:      // 504
			return true
		default:
			return false
		}
	}

	// Network-level failures: timeouts, connection reset/refused (net.OpError),
	// and the *url.Error the http client wraps them in all satisfy net.Error.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// A truncated response body mid-flight is worth one more try.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}
