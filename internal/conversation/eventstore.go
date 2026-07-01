package conversation

import (
	"context"

	"code-agent/internal/session"
)

// ConversationEventStore is the event-log persistence boundary for conversation
// events. It is deliberately separate from ConversationRepository —
// conversations and their events are two different aggregates. This separation
// lets the event store be swapped independently (SQLite → Redis Stream →
// Kafka → S3) without touching the conversation CRUD layer.
type ConversationEventStore interface {
	// Append records one agent event to the session's event log and returns the
	// monotonic seq assigned to it, so the live emitter can broadcast the same seq
	// the replay path reports (v1.2 §4). Best-effort: a write failure must not fail
	// the turn.
	Append(ctx context.Context, e session.EventRecord) (int64, error)

	// Replay returns a session's events in emission order — the raw stream
	// a timeline/index/replay pass consumes.
	Replay(ctx context.Context, sessionID string) ([]session.EventRecord, error)

	// ReplaySince returns events with seq greater than sinceSeq, in seq order —
	// the incremental catch-up a reconnecting client uses (v1.2 §4).
	ReplaySince(ctx context.Context, sessionID string, sinceSeq int64) ([]session.EventRecord, error)
}
