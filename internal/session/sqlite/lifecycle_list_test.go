package sqlite

import (
	"context"
	"testing"
	"time"

	"code-agent/internal/session"
)

// TestListSurfacesTurnLifecycle proves the List projection reads turn_status and
// paused_at out of the persisted metadata JSON so a host can enumerate paused
// sessions without loading each one (v1.2 §3.2).
func TestListSurfacesTurnLifecycle(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	paused := sampleSession()
	paused.ID = "paused-1"
	pausedAt := time.Unix(1_700_000_500, 0)
	paused.MarkPaused(pausedAt)
	if err := store.Save(ctx, paused); err != nil {
		t.Fatalf("save paused: %v", err)
	}

	normal := sampleSession()
	normal.ID = "normal-1"
	if err := store.Save(ctx, normal); err != nil {
		t.Fatalf("save normal: %v", err)
	}

	metas, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]session.Meta{}
	for _, m := range metas {
		byID[m.ID] = m
	}

	if got := byID["paused-1"]; got.TurnStatus != session.TurnStatusPaused {
		t.Errorf("paused session TurnStatus=%q want paused", got.TurnStatus)
	}
	if got := byID["paused-1"]; got.PausedAt != pausedAt.Unix() {
		t.Errorf("paused session PausedAt=%d want %d", got.PausedAt, pausedAt.Unix())
	}
	if got := byID["normal-1"]; got.TurnStatus != "" {
		t.Errorf("normal session TurnStatus=%q want empty", got.TurnStatus)
	}
}
