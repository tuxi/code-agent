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
	Delete(ctx context.Context, id string) error
	Close() error
}

// Meta is a one-line summary of a stored session, for listing.
type Meta struct {
	ID           string
	Model        string
	MessageCount int
	PromptTokens int
	UpdatedAt    time.Time
}
