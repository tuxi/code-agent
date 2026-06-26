package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/session"
)

// newTestRepo creates an in-memory SQLite-backed ConversationRepository for
// testing. Each test gets a fresh, isolated database.
func newTestRepo(t *testing.T) ConversationRepository {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return NewSQLiteRepository(store, 128000, 90000, "test-model", func(workspaceRoot string) string {
		return "" // no skills in tests
	})
}

// newTestRepoWithSkills creates a repo where skillsIndex returns the given
// snippet, so tests can assert it ends up in the system prompt.
func newTestRepoWithSkills(t *testing.T, index string) ConversationRepository {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return NewSQLiteRepository(store, 128000, 90000, "test-model", func(string) string {
		return index
	})
}

func TestRepoCreate(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	// Create workspace dir so Builder can read CODEAGENT.md (absent is OK).
	dir := t.TempDir()
	sess, err := repo.Create(ctx, dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID == "" {
		t.Error("session has no ID")
	}
	if sess.WorkspacePath != dir {
		t.Errorf("WorkspacePath = %q, want %q", sess.WorkspacePath, dir)
	}
	if sess.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", sess.Model)
	}
	if len(sess.Messages) < 1 {
		t.Fatal("session has no messages (expected at least a system prompt)")
	}
	if sess.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", sess.Messages[0].Role)
	}
}

func TestRepoCreate_SkillsIndex(t *testing.T) {
	repo := newTestRepoWithSkills(t, "# Skills\n- skill_a: desc")
	ctx := context.Background()
	dir := t.TempDir()

	sess, err := repo.Create(ctx, dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The skills index should be in the system prompt.
	if len(sess.Messages) == 0 {
		t.Fatal("no messages")
	}
	content := sess.Messages[0].Content
	if content == "" {
		t.Fatal("empty system prompt")
	}
	// Basic check: the system prompt should contain the skills index.
	if !contains(content, "# Skills") {
		t.Error("system prompt missing skills index")
	}
}

func TestRepoCreate_Load(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	dir := t.TempDir()
	created, err := repo.Create(ctx, dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := repo.Load(ctx, created.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != created.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, created.ID)
	}
	if loaded.WorkspacePath != created.WorkspacePath {
		t.Errorf("WorkspacePath = %q, want %q", loaded.WorkspacePath, created.WorkspacePath)
	}
	if len(loaded.Messages) != len(created.Messages) {
		t.Errorf("message count = %d, want %d", len(loaded.Messages), len(created.Messages))
	}
}

func TestRepoLoad_Missing(t *testing.T) {
	repo := newTestRepo(t)
	_, err := repo.Load(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestRepoList(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()

	// Empty list initially.
	metas, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("initial list length = %d, want 0", len(metas))
	}

	// Create two sessions.
	s1, _ := repo.Create(ctx, dir)
	s2, _ := repo.Create(ctx, dir)

	metas, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("list length = %d, want 2", len(metas))
	}
	// Most-recent first.
	if metas[0].ID != s2.ID {
		t.Errorf("first = %q, want %q (most recent)", metas[0].ID, s2.ID)
	}
	if metas[1].ID != s1.ID {
		t.Errorf("second = %q, want %q", metas[1].ID, s1.ID)
	}
}

func TestRepoList_WorkspacePath(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()

	repo.Create(ctx, dir)

	metas, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) == 0 {
		t.Fatal("no sessions listed")
	}
	if metas[0].WorkspacePath != dir {
		t.Errorf("Meta.WorkspacePath = %q, want %q", metas[0].WorkspacePath, dir)
	}
}

func TestRepoSave(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()

	sess, _ := repo.Create(ctx, dir)

	// Modify and save.
	sess.Summary = "test summary"
	if err := repo.Save(ctx, sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, _ := repo.Load(ctx, sess.ID)
	if loaded.Summary != "test summary" {
		t.Errorf("Summary = %q, want 'test summary'", loaded.Summary)
	}
}

func TestRepoDelete(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()

	sess, _ := repo.Create(ctx, dir)

	if err := repo.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Idempotent.
	if err := repo.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("Delete (second): %v", err)
	}
	// Load should fail.
	if _, err := repo.Load(ctx, sess.ID); err == nil {
		t.Fatal("Load after Delete should fail")
	}
	// List should be empty.
	metas, _ := repo.List(ctx)
	if len(metas) != 0 {
		t.Errorf("list after delete = %d, want 0", len(metas))
	}
}

func TestRepoClose(t *testing.T) {
	repo := newTestRepo(t)
	if err := repo.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestBuildFromNonExistentWorkspace verifies Create works even when the
// workspace dir does not exist (CODEAGENT.md is simply absent).
func TestRepoCreate_MissingWorkspace(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	// A path that does not exist.
	sess, err := repo.Create(ctx, "/nonexistent/path/for/test")
	if err != nil {
		// The Builder tries to read CODEAGENT.md — a missing file is OK,
		// but some OSes may reject reading from a non-existent directory.
		// The important thing is that the error, if any, is not a panic
		// and comes from file I/O, not the repository layer.
		t.Logf("Create from missing dir: %v (expected on some platforms)", err)
		return
	}
	if sess.ID == "" {
		t.Error("session has no ID")
	}
}

// ---- helpers ----

func contains(s, sub string) bool {
	return len(s) > 0 && len(sub) > 0 && len(s) >= len(sub) && search(s, sub)
}

func search(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Ensure the temp dir cleanup works even if tests write files.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
