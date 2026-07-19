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

// UploadAsset forwards the Gateway asset lifecycle without applying model-call
// retries. Init/complete are idempotency-bound by Gateway's upload_id; callers
// decide whether a failed screenshot should be retried or reported to the model.
func (p *ResilientProvider) UploadAsset(ctx context.Context, upload AssetUpload) (GatewayAssetRef, error) {
	uploader, ok := p.Inner.(AssetUploader)
	if !ok {
		return GatewayAssetRef{}, errors.New("provider does not support gateway asset uploads")
	}
	return uploader.UploadAsset(ctx, upload)
}

func (p *ResilientProvider) AssetUploadScope(ctx context.Context) string {
	if scoped, ok := p.Inner.(AssetUploadScoper); ok {
		return scoped.AssetUploadScope(ctx)
	}
	return "gateway:unknown"
}

func (p *ResilientProvider) ImageInputCapability(ctx context.Context) (bool, error) {
	prober, ok := p.Inner.(ImageInputCapabilityProber)
	if !ok {
		return false, nil
	}
	return prober.ImageInputCapability(ctx)
}

func (p *ResilientProvider) ReleaseConversationAssetRefs(ctx context.Context, sessionID string) error {
	releaser, ok := p.Inner.(ConversationAssetRefReleaser)
	if !ok {
		return errors.New("provider does not support conversation asset-ref release")
	}
	return releaser.ReleaseConversationAssetRefs(ctx, sessionID)
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
			stat.CachedPromptTokens = resp.Usage.CachedPromptTokens
			stat.CompletionTokens = resp.Usage.CompletionTokens
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
		class := errorClass(err) // "" on success
		stat.Trace = append(stat.Trace, Attempt{Latency: attemptDur, ErrorClass: class})
		if err == nil {
			return resp, nil
		}
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

// CompleteStream streams the inner provider's text and reasoning when supported,
// recording one RequestStat on success. It deliberately does NOT retry a stream:
// a half-emitted stream cannot be cleanly replayed (the renderer already showed
// it). On any failure it falls back to the fully resilient, retried Complete — so
// the result is always recoverable and the resilience guarantee is untouched;
// only the live preview is best-effort.
func (p *ResilientProvider) CompleteStream(ctx context.Context, req Request, onText, onReasoning func(string)) (Response, error) {
	if p.Inner == nil {
		return Response{}, errors.New("resilient provider: nil inner provider")
	}
	sp, ok := p.Inner.(StreamingProvider)
	if !ok {
		return p.Complete(ctx, req) // inner can't stream — fall back to resilient
	}

	attemptCtx := ctx
	var cancel context.CancelFunc
	if p.Timeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, p.Timeout)
	}
	start := time.Now()
	resp, err := sp.CompleteStream(attemptCtx, req, onText, onReasoning)
	if cancel != nil {
		cancel()
	}
	if err == nil {
		if p.Observer != nil {
			p.Observer.Observe(RequestStat{
				At: start, Model: req.Model, Attempts: 1, Success: true,
				Latency:            time.Since(start),
				PromptTokens:       resp.Usage.PromptTokens,
				CachedPromptTokens: resp.Usage.CachedPromptTokens,
				CompletionTokens:   resp.Usage.CompletionTokens,
				Trace:              []Attempt{{Latency: time.Since(start)}},
			})
		}
		return resp, nil
	}
	if ctx.Err() != nil { // caller canceled — terminal, no fallback work
		return Response{}, ctx.Err()
	}
	// A Gateway user-quota error cannot be repaired by switching from stream to
	// non-stream. Returning it directly avoids one needless second request (and
	// any configured retry budget) after the allowance is known to be exhausted.
	if isQuotaExceeded(err) || isUserAssetError(err) {
		return Response{}, err
	}
	p.logf("[provider] stream failed: %s — falling back to non-streamed retry\n", errorClass(err))
	return p.Complete(ctx, req)
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

// IsRetryable exposes the transport-error classifier so higher layers (e.g. the
// /goal pursuit loop) apply the SAME transient-vs-permanent policy the
// ResilientProvider uses internally, instead of re-deriving the 4xx table. A
// permanent error (auth/bad-request) should stop a loop immediately; a transient
// one (timeout/5xx/network) is worth tolerating.
func IsRetryable(err error) bool { return isRetryable(err) }

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
		if isUserAssetError(err) {
			return false
		}
		// Gateway's user quota exhaustion uses HTTP 429, but no amount of
		// backoff can change that user's allowance before reset. Keep ordinary
		// upstream 429s retryable.
		if isQuotaExceeded(err) {
			return false
		}
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

func isUserAssetError(err error) bool {
	_, ok := UserAssetErrorCode(err)
	return ok
}

func isQuotaExceeded(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Code == "quota_exceeded" || apiErr.Type == "quota_exceeded")
}
