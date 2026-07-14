package managedworktree

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/conversation"
	sqlitestore "code-agent/internal/session/sqlite"
	"code-agent/internal/worktree"
)

func TestManagedWorktreeFullStateSurvivesSQLiteRuntimeRestart(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)
	databasePath := filepath.Join(t.TempDir(), "runtime.db")
	store, err := sqlitestore.New(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	newRepo := func(store *sqlitestore.Store) ConversationRepository {
		repo := conversation.NewSQLiteRepository(store, 1000, 800, "test", "", nil)
		managedRepo, ok := repo.(ConversationRepository)
		if !ok {
			t.Fatal("conversation repository does not support reserved managed creation")
		}
		return managedRepo
	}
	req := CreateRequest{
		ClientRequestID: "create_sqlite_restart", SourceWorkspacePath: root,
		SourceWorkspaceID: "main", BaseWorkspaceID: "base", BaseRef: worktree.BaseRefHead,
	}
	created, err := New(store, newRepo(store)).Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlitestore.New(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := New(store, newRepo(store))
	report, err := manager.Reconcile(ctx)
	if err != nil || len(report.Ready) != 1 || len(report.Issues) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	retried, err := manager.Create(ctx, req)
	if err != nil || retried.Session.ID != created.Session.ID || retried.Record.WorktreePath != created.Record.WorktreePath || retried.Record.CheckoutWorkspaceID != created.Record.CheckoutWorkspaceID {
		t.Fatalf("created=%+v retried=%+v err=%v", created.Record, retried.Record, err)
	}
	if count := strings.Count(git(t, root, "worktree", "list", "--porcelain"), "worktree "); count != 2 {
		t.Fatalf("worktree count=%d", count)
	}
}
