package managedworktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/session"
	"code-agent/internal/worktree"
)

func TestReconcileCompletesReservedAndGitCreatedProvisioning(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	repo := newMemoryRepo()
	manager := New(store, repo)

	reserved := recoveryRecord(t, root, "create_reserved", "session-reserved-11111111", "reserved-11111111")
	if _, created, err := store.ReserveWorktree(ctx, reserved); err != nil || !created {
		t.Fatalf("reserve: created=%v err=%v", created, err)
	}

	createdByGit := recoveryRecord(t, root, "create_git_done", "session-created-22222222", "created-22222222")
	createdByGit.State = worktree.StateProvisioning
	if _, created, err := store.ReserveWorktree(ctx, createdByGit); err != nil || !created {
		t.Fatalf("reserve git-created: created=%v err=%v", created, err)
	}
	if err := os.MkdirAll(filepath.Dir(createdByGit.WorktreePath), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "worktree", "add", "-b", createdByGit.Branch, createdByGit.WorktreePath, createdByGit.BaseCommit)

	report, err := manager.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Ready) != 2 || len(report.Failed) != 0 {
		t.Fatalf("report=%+v", report)
	}
	for _, record := range []worktree.Record{reserved, createdByGit} {
		stored, err := store.WorktreeBySessionID(ctx, record.SessionID)
		if err != nil || stored.State != worktree.StateReady {
			t.Fatalf("stored=%+v err=%v", stored, err)
		}
		sess, err := repo.Load(ctx, record.SessionID)
		if err != nil || sess.WorkspacePath != record.WorktreePath || sess.ExecutionPolicy() != session.ExecutionPolicyIsolatedWorktree {
			t.Fatalf("session=%+v err=%v", sess, err)
		}
	}
}

func TestReconcileMarksManuallyDeletedCheckoutMissingWithoutRecreating(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	repo := newMemoryRepo()
	manager := New(store, repo)
	req := CreateRequest{ClientRequestID: "create_missing", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead}
	created, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(created.Record.WorktreePath); err != nil {
		t.Fatal(err)
	}
	report, err := manager.Reconcile(ctx)
	if err != nil || len(report.Missing) != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, statErr := os.Stat(created.Record.WorktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("missing checkout was recreated: %v", statErr)
	}
	stored, _ := store.WorktreeBySessionID(ctx, created.Session.ID)
	if stored.State != worktree.StateMissing || stored.LastErrorCode != CodeMissing {
		t.Fatalf("stored=%+v", stored)
	}
	sess, err := repo.Load(ctx, created.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := sess.Metadata[session.MetaManagedWorktree].(map[string]any)
	if meta["state"] != string(worktree.StateMissing) || meta["needs_rebind"] != true {
		t.Fatalf("metadata=%+v", meta)
	}
	_, err = manager.Create(ctx, req)
	var managedErr *Error
	if !errors.As(err, &managedErr) || managedErr.Code != CodeMissing {
		t.Fatalf("repeat create err=%v", err)
	}
}

func TestReconcileReportsOrphanWithoutDeletingIt(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	manager := New(store, newMemoryRepo())
	if _, err := manager.Create(ctx, CreateRequest{ClientRequestID: "known", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead}); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(root, ".codeagent", "worktrees", "manual-orphan")
	git(t, root, "worktree", "add", "-b", "codeagent/manual-orphan", orphanPath, "HEAD")
	report, err := manager.Reconcile(ctx)
	if err != nil || len(report.Orphans) != 1 || pathIdentity(report.Orphans[0].WorktreePath) != pathIdentity(orphanPath) {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(orphanPath, "tracked.txt")); err != nil {
		t.Fatalf("orphan was removed: %v", err)
	}
}

func TestRemoveRejectsRiskAndPreservesBranchAndConversation(t *testing.T) {
	ctx := context.Background()
	for _, mutation := range []string{"dirty", "untracked", "commit"} {
		t.Run(mutation, func(t *testing.T) {
			root := initGitRepo(t)
			store := session.NewMemoryStore()
			repo := newMemoryRepo()
			manager := New(store, repo)
			created, err := manager.Create(ctx, CreateRequest{ClientRequestID: "create_" + mutation, SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead})
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "dirty":
				if err := os.WriteFile(filepath.Join(created.Record.WorktreePath, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "untracked":
				if err := os.WriteFile(filepath.Join(created.Record.WorktreePath, "new.txt"), []byte("new\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "commit":
				if err := os.WriteFile(filepath.Join(created.Record.WorktreePath, "committed.txt"), []byte("new\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				git(t, created.Record.WorktreePath, "add", "committed.txt")
				git(t, created.Record.WorktreePath, "commit", "-m", "new commit")
			}
			_, err = manager.Remove(ctx, RemoveRequest{SessionID: created.Session.ID, RequestID: "remove_" + mutation})
			var managedErr *Error
			if !errors.As(err, &managedErr) || managedErr.Code != CodeDirty {
				t.Fatalf("err=%v", err)
			}
			if _, err := os.Stat(created.Record.WorktreePath); err != nil {
				t.Fatalf("risky checkout was removed: %v", err)
			}
		})
	}

	root := initGitRepo(t)
	store := session.NewMemoryStore()
	repo := newMemoryRepo()
	manager := New(store, repo)
	created, err := manager.Create(ctx, CreateRequest{ClientRequestID: "create_clean", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := manager.Remove(ctx, RemoveRequest{SessionID: created.Session.ID, RequestID: "remove_clean"})
	if err != nil || removed.Record.State != worktree.StateRemoved {
		t.Fatalf("removed=%+v err=%v", removed, err)
	}
	if _, err := os.Stat(created.Record.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("checkout remains: %v", err)
	}
	if got := strings.TrimSpace(git(t, root, "show-ref", "--verify", "refs/heads/"+created.Record.Branch)); got == "" {
		t.Fatal("worktree branch was deleted")
	}
	if _, err := repo.Load(ctx, created.Session.ID); err != nil {
		t.Fatalf("conversation was deleted: %v", err)
	}
	duplicate, err := manager.Remove(ctx, RemoveRequest{SessionID: created.Session.ID, RequestID: "remove_clean"})
	if err != nil || duplicate.Record.State != worktree.StateRemoved {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
}

func TestRemoveForceAndBusyGuard(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	manager := New(store, newMemoryRepo())
	created, err := manager.Create(ctx, CreateRequest{ClientRequestID: "create_force", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetBusyChecker(func(sessionID string) bool { return sessionID == created.Session.ID })
	_, err = manager.Remove(ctx, RemoveRequest{SessionID: created.Session.ID, RequestID: "busy", Force: true})
	var managedErr *Error
	if !errors.As(err, &managedErr) || managedErr.Code != CodeInUse {
		t.Fatalf("busy err=%v", err)
	}
	manager.SetBusyChecker(nil)
	if err := os.WriteFile(filepath.Join(created.Record.WorktreePath, "new.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := manager.Remove(ctx, RemoveRequest{SessionID: created.Session.ID, RequestID: "force", Force: true})
	if err != nil || removed.Record.State != worktree.StateRemoved || !removed.Record.RemoveForce {
		t.Fatalf("removed=%+v err=%v", removed, err)
	}
	_, err = manager.Remove(ctx, RemoveRequest{SessionID: created.Session.ID, RequestID: "force", Force: false})
	if !errors.As(err, &managedErr) || managedErr.Code != CodeRequestConflict {
		t.Fatalf("force mismatch err=%v", err)
	}
}

func TestTurnGuardAtomicallyRejectsRemoval(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	manager := New(store, newMemoryRepo())
	created, err := manager.Create(ctx, CreateRequest{ClientRequestID: "create_guard", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead})
	if err != nil {
		t.Fatal(err)
	}
	release, err := manager.AcquireTurnGuard(ctx, created.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Remove(ctx, RemoveRequest{SessionID: created.Session.ID, RequestID: "remove_guard", Force: true})
	var managedErr *Error
	if !errors.As(err, &managedErr) || managedErr.Code != CodeInUse {
		t.Fatalf("remove err=%v", err)
	}
	release()
	if _, err := manager.Remove(ctx, RemoveRequest{SessionID: created.Session.ID, RequestID: "remove_guard", Force: true}); err != nil {
		t.Fatalf("remove after guard release: %v", err)
	}
}

func TestReconcileResumesDurableRemovingState(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	repo := newMemoryRepo()
	created, err := New(store, repo).Create(ctx, CreateRequest{ClientRequestID: "create_removing", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Record.WorktreePath, "new.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := created.Record
	record.State = worktree.StateRemoving
	record.RemoveRequestID = "remove_crash"
	record.RemoveForce = true
	record.UpdatedAt = time.Now().UTC()
	if err := store.UpdateWorktree(ctx, record); err != nil {
		t.Fatal(err)
	}
	report, err := New(store, repo).Reconcile(ctx)
	if err != nil || len(report.Removed) != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := os.Stat(record.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("removing state was not resumed: %v", err)
	}
}

func TestConversationDeletionDoesNotRemoveManagedWorktree(t *testing.T) {
	ctx := context.Background()
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	repo := newMemoryRepo()
	manager := New(store, repo)
	created, err := manager.Create(ctx, CreateRequest{ClientRequestID: "create_keep", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead})
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	delete(repo.sessions, created.Session.ID)
	repo.mu.Unlock()
	if _, err := os.Stat(created.Record.WorktreePath); err != nil {
		t.Fatalf("conversation deletion removed checkout: %v", err)
	}
	record, err := store.WorktreeBySessionID(ctx, created.Session.ID)
	if err != nil || record.State != worktree.StateReady {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func recoveryRecord(t *testing.T, root, requestID, sessionID, name string) worktree.Record {
	t.Helper()
	now := time.Now().UTC()
	return worktree.Record{
		SessionID: sessionID, ClientRequestID: requestID,
		CheckoutWorkspaceID: "checkout_" + name,
		SourceWorkspacePath: root,
		WorktreePath:        filepath.Join(root, ".codeagent", "worktrees", name),
		Name:                name,
		Branch:              "codeagent/" + name,
		BaseRef:             worktree.BaseRefHead,
		BaseCommit:          strings.TrimSpace(git(t, root, "rev-parse", "HEAD")),
		State:               worktree.StateReserved,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
}
