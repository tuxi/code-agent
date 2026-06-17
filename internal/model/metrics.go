package model

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// RequestStat summarizes one Complete call — across all of its retry attempts —
// for transport telemetry. ResilientProvider emits it to an Observer when one is
// set. This is the data behind "why are requests slow / failing", which a bare
// "context deadline exceeded" cannot answer.
type RequestStat struct {
	At               time.Time
	Model            string
	PromptTokens     int  // from the successful response; 0 on failure
	CompletionTokens int  // output tokens, for cost accounting
	Attempts         int  // inner calls made (>= 1)
	Retries          int  // Attempts - 1
	TimedOut         bool // any attempt hit a timeout
	Success          bool
	ErrorClass       string        // "" on success; else timeout/429/5xx/4xx/network/...
	Latency          time.Duration // total wall time across attempts
	Trace            []Attempt     // per-attempt detail, in order
}

// Attempt is one try within a Complete call — the per-attempt detail behind the
// request trace ("attempt 1: 30s timeout / attempt 2: 5s success").
type Attempt struct {
	Latency    time.Duration
	ErrorClass string // "" on success
}

// Observer receives one RequestStat per Complete call. Implementations must be
// best-effort: telemetry must never fail or meaningfully slow a request.
type Observer interface {
	Observe(RequestStat)
}

// errorClass labels an error for telemetry. It is coarser than isRetryable: it
// groups by cause (timeout vs 429 vs 5xx vs network) so the stats explain WHY
// requests fail, not just whether to retry.
func errorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 429:
			return "429"
		case apiErr.StatusCode >= 500:
			return "5xx"
		case apiErr.StatusCode >= 400:
			return "4xx"
		default:
			return fmt.Sprintf("http_%d", apiErr.StatusCode)
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "error"
}
