package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/worktree"
)

func TestManagedWorktreeReservationSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	record := worktree.Record{
		ClientRequestID: "create_restart", SessionID: "session_a",
		BaseWorkspaceID: "base", SourceWorkspaceID: "main", CheckoutWorkspaceID: "checkout_a",
		SourceWorkspacePath: "/repo", WorktreePath: "/repo/.codeagent/worktrees/a",
		Name: "a", Branch: "codeagent/a", BaseRef: worktree.BaseRefHead,
		State: worktree.StateProvisioning, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	stored, created, err := store.ReserveWorktree(context.Background(), record)
	if err != nil || !created || stored.SessionID != record.SessionID {
		t.Fatalf("reserve stored=%+v created=%v err=%v", stored, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stored, created, err = store.ReserveWorktree(context.Background(), worktree.Record{ClientRequestID: record.ClientRequestID})
	if err != nil || created || stored.SessionID != record.SessionID || stored.WorktreePath != record.WorktreePath || stored.State != worktree.StateProvisioning {
		t.Fatalf("restart reserve stored=%+v created=%v err=%v", stored, created, err)
	}
}
