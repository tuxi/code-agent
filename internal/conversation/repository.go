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
	// already baked in. An empty workspacePath means "server default." workspaceExtID
	// is the host's stable identifier for an external (outside-workspaceDir) workspace
	// — an iOS security-scoped-bookmark id — and is empty for workspace-local paths
	// and on desktop. See docs/ios_workspace_path_spec.md §6.1.
	Create(ctx context.Context, workspacePath, workspaceExtID string) (*session.Session, error)

	// Load returns the full session by id, or an error if not found.
	Load(ctx context.Context, id string) (*session.Session, error)

	// Rebind records a host-supplied fresh absolute path for an external session's
	// workspace, valid for THIS process launch only (it does not change the persisted
	// ref). It validates the path exists. Idempotent — the host may call it for any
	// external session before opening the stream. See spec §6.2bis.
	Rebind(ctx context.Context, id, absPath string) error

	// NeedsRebind reports whether a session's workspace is external and not yet
	// resolvable this launch, so the host must Rebind before any turn runs. It is
	// always false for workspace-rooted sessions (the runtime self-anchors) and on
	// desktop. Drives the detail endpoint's needs_rebind flag.
	NeedsRebind(ctx context.Context, id string) (bool, error)

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
