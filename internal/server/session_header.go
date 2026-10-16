package server

import (
	"context"

	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

// loadConversationHeader is only for read-only identity/metadata checks.
// Older repositories keep their full-Load fallback; execution still uses Load.
func loadConversationHeader(ctx context.Context, repo conversation.ConversationRepository, id string) (*session.Session, error) {
	if headers, ok := repo.(session.SessionHeaderStore); ok {
		return headers.LoadHeader(ctx, id)
	}
	return repo.Load(ctx, id)
}
