package sessionfork

import "context"

// Service is the application boundary for persistent conversation forks.
// Tool and HTTP adapters share it; Runtime ownership and transport stay behind
// the implementation.
type Service interface {
	ForkSession(context.Context, Request) (Result, error)
}

type Request struct {
	ParentSessionID string
	ParentTurnID    string
	SourceSessionID string
	RequestID       string
	Name            string
	ExecutionPolicy string
	WorktreeName    string
	// ThroughTurnID, when set, forks only the source history through the end of
	// that turn (the user message's OriginTurnID). Empty keeps the whole
	// checkpoint. omitempty keeps the marshaled payload — and therefore the
	// control-plane spawn-edge idempotency hash — unchanged for existing callers.
	ThroughTurnID string `json:"through_turn_id,omitempty"`
}

type Result struct {
	ID              string `json:"id"`
	ParentSessionID string `json:"parent_session_id"`
	SourceSessionID string `json:"source_session_id"`
	WorkspacePath   string `json:"workspace_path"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	BaseCommit      string `json:"base_commit,omitempty"`
	// ThroughTurnID echoes the fork cut so a client can reconstruct the inherited
	// prefix from the source conversation.
	ThroughTurnID string `json:"through_turn_id,omitempty"`
}
