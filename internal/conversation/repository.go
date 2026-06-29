package conversation

import (
	"context"

	"code-agent/internal/session"
)

// ConversationRepository is the persistence boundary for conversation metadata.
// It is backed by SQLite (session.Store) and is the single source of truth:
// the HTTP API reads directly from it; TurnExecutor loads and saves through it.
// No in-memory cache sits between callers and this interface.
//
// Note: Events are deliberately NOT part of this interface. They belong to
// ConversationEventStore — a separate aggregate (EventLog / Replay / Timeline)
// that can be swapped independently (SQLite → Redis Stream → Kafka → S3).
type ConversationRepository interface {
	// Create builds a new session with the given workspace path and persists it.
	// The returned session has a system prompt, project memory, and skills index
	// already baked in. An empty workspacePath means "server default."
	Create(ctx context.Context, workspacePath string) (*session.Session, error)

	// Load returns the full session by id, or an error if not found.
	Load(ctx context.Context, id string) (*session.Session, error)

	// Save persists a session. Called after every turn (best-effort autosave)
	// and when a session's metadata changes.
	Save(ctx context.Context, s *session.Session) error

	// List returns metadata for every stored session, most-recently-updated first.
	List(ctx context.Context) ([]session.Meta, error)

	// Delete removes a session and its messages/compactions/events. Idempotent.
	Delete(ctx context.Context, id string) error

	// UpdateName changes the display name of a session without loading/saving
	// the full session. Used by the PATCH rename endpoint and async title generation.
	UpdateName(ctx context.Context, id string, name string) error

	// Close releases resources (e.g. the underlying SQLite connection).
	Close() error
}
