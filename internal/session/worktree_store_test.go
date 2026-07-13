package session

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code-agent/internal/worktree"
)

func TestMemoryStoreReserveWorktreeIsConcurrentIdempotent(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan worktree.Record, 16)
	var created atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			record, wasCreated, err := store.ReserveWorktree(ctx, worktree.Record{
				ClientRequestID:     "create_same",
				SessionID:           fmt.Sprintf("session_%d", i),
				CheckoutWorkspaceID: fmt.Sprintf("checkout_%d", i),
				WorktreePath:        fmt.Sprintf("/repo/.codeagent/worktrees/wt-%d", i),
				Branch:              fmt.Sprintf("codeagent/wt-%d", i),
				Name:                fmt.Sprintf("wt-%d", i),
				State:               worktree.StateReserved,
				CreatedAt:           time.Now(), UpdatedAt: time.Now(),
			})
			if err != nil {
				t.Errorf("ReserveWorktree: %v", err)
				return
			}
			if wasCreated {
				created.Add(1)
			}
			results <- record
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	if created.Load() != 1 {
		t.Fatalf("created=%d want 1", created.Load())
	}
	var sessionID string
	for result := range results {
		if sessionID == "" {
			sessionID = result.SessionID
		}
		if result.SessionID != sessionID {
			t.Fatalf("duplicate request returned different sessions: %q vs %q", sessionID, result.SessionID)
		}
	}
}
