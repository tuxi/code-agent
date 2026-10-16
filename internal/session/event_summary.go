package session

import (
	"context"
	"time"
)

// EventSummary is the metadata needed by a conversation detail response.
// FirstAt and LastAt follow seq order, matching event replay.
type EventSummary struct {
	HasEvents    bool
	FirstAt      time.Time
	LastAt       time.Time
	TurnCount    int
	MessageCount int
}

// EventSummaryStore avoids materializing event payloads just to render detail
// counts and timestamps.
type EventSummaryStore interface {
	SessionEventSummary(ctx context.Context, sessionID string) (EventSummary, error)
}
