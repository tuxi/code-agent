package session

import (
	"context"
	"encoding/json"
	"time"
)

// SessionStore is the core persistence port for conversation CRUD. It is the
// ONLY interface the Agent Runtime requires — consumers who swap the storage
// backend must implement at least this. The agent loop never touches a Store
// directly; callers save at turn boundaries, where the message sequence is
// always consistent (a tool-result is never orphaned from its assistant
// tool_calls), so a resumed session is always valid to send to the model.
type SessionStore interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context) ([]Meta, error)
	Delete(ctx context.Context, id string) error
	UpdateName(ctx context.Context, id string, name string) error
	Close() error
}

// EventStore is the optional event-log persistence port. It records and replays
// agent events (the P7 EventStore — the raw, replayable runtime stream).
// Consumers that don't need timeline replay or event search can provide a no-op
// implementation. Best-effort by convention: a write failure must not fail a run.
type EventStore interface {
	// RecordEvent appends one agent event to the per-session event log.
	// SessionEvents reads them back in emission order — the foundation for
	// timeline replay/search/analytics.
	RecordEvent(ctx context.Context, e EventRecord) error
	SessionEvents(ctx context.Context, sessionID string) ([]EventRecord, error)

	// RecentEventsByKind returns the most recent events of one kind across all
	// sessions, newest first. It indexes a class of event without scanning every
	// session — e.g. each subagent delegation writes a task_started event, so this
	// lists recent delegations (and their sub-session ids) for `codeagent tasks`.
	RecentEventsByKind(ctx context.Context, kind string, limit int) ([]EventRecord, error)
}

// TelemetryStore is the optional observability persistence port. The
// stats/trace/cost CLI commands depend on it; consumers that don't need
// transport telemetry can provide a no-op implementation.
type TelemetryStore interface {
	Stats(ctx context.Context) (Stats, error)
	RecordRequest(ctx context.Context, r RequestRecord) error
	ProviderStats(ctx context.Context) (ProviderStats, error)
	RecentRequests(ctx context.Context, limit int) ([]RequestRecord, error)
	TokenUsageByModel(ctx context.Context) ([]ModelUsage, error)
}

// Store is the combined persistence interface — backward compatible with all
// existing code. It composes SessionStore, EventStore, and TelemetryStore.
// New consumers should depend on only the interface they need (e.g. a
// conversation repository depends on SessionStore, an event emitter on
// EventStore, a stats reporter on TelemetryStore).
type Store interface {
	SessionStore
	EventStore
	TelemetryStore
}

// StoreFactory creates a Store for a given workspace root. It is the injection
// seam that lets external consumers (Flux, DreamAI) swap the storage backend
// without forking code-agent.
//
// The root parameter is the absolute workspace directory. The factory decides
// how to map this to a concrete store: the default SQLite factory derives a
// per-project path under ~/.codeagent; a Postgres factory might use root only
// for namespace derivation; an in-memory factory ignores it entirely.
type StoreFactory func(root string) (Store, error)

// EventRecord is one persisted agent event — the raw, *unfolded* runtime stream.
// Where messages capture the model's view of a conversation, events capture the
// process: tool calls, observations, reflections, skills, compaction. The folded
// timeline a UI shows is a projection of these; replay/search/export need the
// originals, so the event — not the projection — is what we persist. Payload is
// the full event as JSON (the source of truth); Kind/At are denormalized as the
// query index.
type EventRecord struct {
	SessionID string
	TurnID    string
	Kind      string
	At        time.Time
	Payload   json.RawMessage
}

// RequestRecord is one persisted model request (across its retry attempts) for
// transport telemetry. The persisted log doubles as a per-request trace.
type RequestRecord struct {
	At                 time.Time
	Model              string
	PromptTokens       int
	CachedPromptTokens int
	CompletionTokens   int
	Attempts           int
	Retries            int
	TimedOut           bool
	Success            bool
	ErrorClass         string
	LatencyMs          int64
	Trace              []AttemptRecord // per-attempt detail
}

// ModelUsage is per-model token totals — the basis for cost (tokens × the
// model's configured price, computed by the caller that holds the prices).
type ModelUsage struct {
	Model              string
	Requests           int
	PromptTokens       int64
	CachedPromptTokens int64
	CompletionTokens   int64
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
	Name         string // persisted display name (auto-generated or user-set); empty falls back to Title
	Title        string // derived fallback: first user message, truncated — a human label for pickers
	Model        string
	MessageCount int
	PromptTokens int
	UpdatedAt    time.Time

	WorkspacePath string // absolute project root directory

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

	// MaxPromptTokens is the largest prompt_tokens across all sessions — shows
	// how close the busiest session has come to its compaction threshold.
	MaxPromptTokens int
	// MaxCompactThreshold is the compact_threshold of the session with the
	// highest prompt_tokens, so the "how full" display is always paired with
	// the right threshold.
	MaxCompactThreshold int
}
