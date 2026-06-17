package session

import (
	"context"
	"time"
)

// Store persists sessions so a conversation survives process exit: resume loads
// the full history — including the expensive LLM summary and the compaction
// trace — instead of starting over.
//
// Persistence is an application-layer concern: the agent loop never touches a
// Store. Callers save at turn boundaries, where the message sequence is always
// consistent (a tool-result is never orphaned from its assistant tool_calls),
// so a resumed session is always valid to send to the model.
type Store interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context) ([]Meta, error)
	Stats(ctx context.Context) (Stats, error)
	RecordRequest(ctx context.Context, r RequestRecord) error
	ProviderStats(ctx context.Context) (ProviderStats, error)
	RecentRequests(ctx context.Context, limit int) ([]RequestRecord, error)
	Delete(ctx context.Context, id string) error
	Close() error
}

// RequestRecord is one persisted model request (across its retry attempts) for
// transport telemetry. The persisted log doubles as a per-request trace.
type RequestRecord struct {
	At           time.Time
	Model        string
	PromptTokens int
	Attempts     int
	Retries      int
	TimedOut     bool
	Success      bool
	ErrorClass   string
	LatencyMs    int64
	Trace        []AttemptRecord // per-attempt detail
}

// AttemptRecord is one try within a request, for the per-attempt trace.
type AttemptRecord struct {
	LatencyMs int64  `json:"ms"`
	Result    string `json:"result"` // "success" or an error class
}

// ProviderStats is aggregate transport telemetry across all recorded requests —
// the evidence behind "why are requests slow / failing", which a bare
// "context deadline exceeded" cannot answer. Percentiles and the histogram show
// the latency DISTRIBUTION, not just the average (which hides the slow tail).
type ProviderStats struct {
	Requests     int
	Successes    int
	Failures     int
	Timeouts     int
	Retries      int
	AvgLatencyMs float64
	MaxLatencyMs int64
	P50LatencyMs int64
	P95LatencyMs int64
	P99LatencyMs int64
	Histogram    []LatencyBucket
}

// LatencyBucket is one bar of the latency histogram: how many requests fell in
// [previous bound, UpperMs). The last bucket's UpperMs is the max int64 (the
// "and above" bucket).
type LatencyBucket struct {
	Label   string
	UpperMs int64
	Count   int
}

// Meta is a one-line summary of a stored session, for listing. The compaction
// fields are aggregated from the session's compactions, not stored separately.
type Meta struct {
	ID           string
	Model        string
	MessageCount int
	PromptTokens int
	UpdatedAt    time.Time

	Compactions   int       // number of finalized compactions
	TotalSaved    int       // total tokens reclaimed across them
	LastCompacted time.Time // zero if never compacted
}

// Stats is aggregate compaction telemetry across all stored sessions — the real
// numbers (compression ratio, summary size) needed to size a token-based recent
// window on evidence instead of a guess. Only finalized compactions are counted.
type Stats struct {
	Sessions        int
	Compactions     int
	AvgBefore       float64
	AvgAfter        float64
	AvgSaved        float64
	AvgRatio        float64
	AvgSummaryChars float64
	MaxRatio        float64
	MinRatio        float64
}
