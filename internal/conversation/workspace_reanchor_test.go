package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
)

// End-to-end re-anchor: a session created under one workspaceDir resolves, through
// the real SQLite store, to the equivalent path under a *different* workspaceDir on
// the next launch — the iOS reinstall case. Both repos share one DB file (the DB
// itself is opened fresh each launch; only its absolute workspace path was poison).
func TestRepo_ReanchorAcrossReinstall(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	oldBase := t.TempDir()
	newBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(oldBase, "MyProj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(newBase, "MyProj"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Launch 1: create a session for oldBase/MyProj.
	store1, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repo1 := NewSQLiteRepository(store1, 128000, 90000, "m", oldBase, func(string) string { return "" })
	sess, err := repo1.Create(ctx, filepath.Join(oldBase, "MyProj"), "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Workspace.Root != session.RootWorkspace || sess.Workspace.Rel != "MyProj" {
		t.Fatalf("create ref = %+v, want workspace/MyProj", sess.Workspace)
	}
	id := sess.ID
	store1.Close()

	// Launch 2: same DB, new container path. Load must re-anchor under newBase.
	store2, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	repo2 := NewSQLiteRepository(store2, 128000, 90000, "m", newBase, func(string) string { return "" })

	loaded, err := repo2.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(newBase, "MyProj")
	if loaded.WorkspacePath != want {
		// normalizePath may add a /private prefix; compare resolved forms.
		if rl, _ := filepath.EvalSymlinks(loaded.WorkspacePath); rl != mustEval(t, want) {
			t.Fatalf("re-anchored WorkspacePath = %q, want %q", loaded.WorkspacePath, want)
		}
	}

	// List must re-anchor too.
	metas, err := repo2.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("List returned %d, want 1", len(metas))
	}
	if mustEval(t, metas[0].WorkspacePath) != mustEval(t, want) {
		t.Fatalf("List WorkspacePath = %q, want under %q", metas[0].WorkspacePath, want)
	}
}

// An external workspace (outside workspaceDir) needs a host rebind on a fresh launch:
// NeedsRebind is true until Rebind supplies a path, after which Load resolves to it.
func TestRepo_ExternalRebind(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	workspaceDir := t.TempDir() // Documents

	oldExt := t.TempDir() // an external folder this launch
	newExt := t.TempDir() // the same folder resolved on a later launch
	if err := os.MkdirAll(oldExt, 0o755); err != nil {
		t.Fatal(err)
	}

	// Launch 1: create external session with a host bookmark id.
	store1, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repo1 := NewSQLiteRepository(store1, 128000, 90000, "m", workspaceDir, func(string) string { return "" })
	sess, err := repo1.Create(ctx, oldExt, "BKMK-7f3a")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Workspace.Root != session.RootExternal || sess.Workspace.ExtID != "BKMK-7f3a" {
		t.Fatalf("create ref = %+v, want external/BKMK-7f3a", sess.Workspace)
	}
	id := sess.ID
	store1.Close()

	// Launch 2: fresh process. The bookmark id is not an absolute path, so the
	// session needs a rebind before it can resolve.
	store2, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	repo2 := NewSQLiteRepository(store2, 128000, 90000, "m", workspaceDir, func(string) string { return "" })

	if nr, err := repo2.NeedsRebind(ctx, id); err != nil || !nr {
		t.Fatalf("NeedsRebind = %v (err %v), want true", nr, err)
	}

	// Rebind to a non-existent path → rejected.
	if err := repo2.Rebind(ctx, id, filepath.Join(newExt, "gone")); err == nil {
		t.Fatal("Rebind to a non-existent path should fail")
	}
	// Rebind to the fresh absolute path → resolvable.
	if err := repo2.Rebind(ctx, id, newExt); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if nr, err := repo2.NeedsRebind(ctx, id); err != nil || nr {
		t.Fatalf("NeedsRebind after rebind = %v (err %v), want false", nr, err)
	}
	loaded, err := repo2.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if mustEval(t, loaded.WorkspacePath) != mustEval(t, newExt) {
		t.Fatalf("WorkspacePath = %q, want %q", loaded.WorkspacePath, newExt)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
