package gitworkspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBranchLifecycleAndDirtyProtection(t *testing.T) {
	root := newRepository(t)
	m := New(root, nil, nil)
	result, err := m.List(context.Background(), root)
	if err != nil || len(result.Branches) != 1 || result.Branches[0].Name != "main" {
		t.Fatalf("list=%+v err=%v", result, err)
	}
	result, err = m.Create(context.Background(), root, "feature/ui", "", false, "req-1")
	if err != nil || len(result.Branches) != 2 {
		t.Fatalf("create=%+v err=%v", result, err)
	}
	retry, err := m.Create(context.Background(), root, "feature/ui", "", false, "req-1")
	if err != nil || len(retry.Branches) != 2 {
		t.Fatalf("idempotent retry=%+v err=%v", retry, err)
	}
	if _, err = m.Checkout(context.Background(), root, "feature/ui", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Checkout(context.Background(), root, "main", false); err == nil || !hasCode(err, CodeDirty) {
		t.Fatalf("dirty checkout err=%v", err)
	}
	if _, err = m.Create(context.Background(), root, "feature/dirty", "", true, ""); err == nil || !hasCode(err, CodeDirty) {
		t.Fatalf("dirty create err=%v", err)
	}
}

func TestNonGitAndUnauthorized(t *testing.T) {
	root := t.TempDir()
	m := New(root, nil, nil)
	result, err := m.List(context.Background(), root)
	if err != nil || result.Checkout.IsGitRepository {
		t.Fatalf("non-git=%+v err=%v", result, err)
	}
	outside := t.TempDir()
	if _, err = m.List(context.Background(), outside); err == nil || !hasCode(err, CodeWorkspaceNotAuthorized) {
		t.Fatalf("unauthorized err=%v", err)
	}
}

func TestInvalidRefAndMissingBranch(t *testing.T) {
	root := newRepository(t)
	m := New(root, nil, nil)
	if _, err := m.Create(context.Background(), root, "bad name", "", false, ""); err == nil || !hasCode(err, CodeInvalidRef) {
		t.Fatalf("invalid ref err=%v", err)
	}
	if _, err := m.Checkout(context.Background(), root, "missing", false); err == nil || !hasCode(err, CodeBranchNotFound) {
		t.Fatalf("missing branch err=%v", err)
	}
}

func hasCode(err error, code string) bool { e, ok := err.(*Error); return ok && e.Code == code }

func newRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "README")
	git(t, root, "commit", "-m", "initial")
	return root
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
