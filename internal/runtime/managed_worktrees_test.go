package runtime

import (
	"context"
	"testing"

	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

type sessionStoreWithoutWorktrees struct{ session.Store }

func TestConfigureManagedWorktreesUsesActualRuntimeSupport(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	repo := conversation.NewSQLiteRepository(store, 1000, 800, "test", "", nil)
	executor := conversation.NewTurnExecutor(repo, &conversation.StoreEventAdapter{Store: store}, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), nil)

	manager, report, err := ConfigureManagedWorktrees(ctx, store, repo, executor, true)
	if err != nil || manager == nil || len(report.Issues) != 0 {
		t.Fatalf("manager=%v report=%+v err=%v", manager, report, err)
	}
	manager, _, err = ConfigureManagedWorktrees(ctx, sessionStoreWithoutWorktrees{Store: store}, repo, executor, true)
	if err != nil || manager != nil {
		t.Fatalf("unsupported store manager=%v err=%v", manager, err)
	}
	manager, _, err = ConfigureManagedWorktrees(ctx, store, repo, executor, false)
	if err != nil || manager != nil {
		t.Fatalf("sandboxed manager=%v err=%v", manager, err)
	}
}
