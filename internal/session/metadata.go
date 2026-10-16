package session

import "context"

// SessionMetadataStore omits message bodies and aggregate subqueries. It is
// used by lifecycle/activity/ownership projections, not the legacy full list.
type SessionMetadataStore interface {
	ListMetadata(context.Context) ([]Meta, error)
}

// SessionHeaderStore reads only identity, display, workspace and lifecycle
// fields. The returned Session has no message/compaction/ledger data and must
// only be used for read-only metadata, never execution or persistence.
type SessionHeaderStore interface {
	LoadHeader(context.Context, string) (*Session, error)
}

type SessionListCursor struct {
	UpdatedAt string `json:"updated_at"`
	ID        string `json:"id"`
	Archived  bool   `json:"archived"`
}

type SessionPageStore interface {
	ListSessionPage(context.Context, bool, *SessionListCursor, int) ([]Meta, *SessionListCursor, error)
}

type InterruptedSessionStore interface {
	PauseInterruptedSessions(context.Context) error
}
