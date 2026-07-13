package managedworktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"code-agent/internal/session"
	"code-agent/internal/worktree"
)

type memoryRepo struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
}

func newMemoryRepo() *memoryRepo { return &memoryRepo{sessions: map[string]*session.Session{}} }

func (r *memoryRepo) CreateWithID(_ context.Context, id, workspacePath, _ string) (*session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess := &session.Session{ID: id, WorkspacePath: workspacePath, Metadata: map[string]any{}}
	r.sessions[id] = sess
	return sess, nil
}
func (r *memoryRepo) Load(_ context.Context, id string) (*session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.sessions[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	copy := *sess
	return &copy, nil
}
func (r *memoryRepo) Save(_ context.Context, sess *session.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *sess
	r.sessions[sess.ID] = &copy
	return nil
}

func TestCreateManagedWorktreeIsOptInIdempotentAndClean(t *testing.T) {
	root := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := session.NewMemoryStore()
	repo := newMemoryRepo()
	manager := New(store, repo)
	req := CreateRequest{
		ClientRequestID: "create_same", SourceWorkspacePath: root,
		SourceWorkspaceID: "main", BaseWorkspaceID: "project",
		SuggestedName: "Multi Session Cache", BaseRef: worktree.BaseRefHead,
	}

	const callers = 8
	results := make(chan CreateResult, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := manager.Create(context.Background(), req)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first CreateResult
	for result := range results {
		if first.Session == nil {
			first = result
			continue
		}
		if result.Session.ID != first.Session.ID || result.Record.WorktreePath != first.Record.WorktreePath {
			t.Fatalf("idempotent create diverged: %+v / %+v", first.Record, result.Record)
		}
	}
	if first.Record.State != worktree.StateReady || !strings.HasPrefix(first.Record.Name, "multi-session-cache-") || first.Record.Branch != "codeagent/"+first.Record.Name {
		t.Fatalf("record=%+v", first.Record)
	}
	if len(first.Warnings) != 1 || first.Warnings[0].Code != "source_workspace_dirty" {
		t.Fatalf("warnings=%+v", first.Warnings)
	}
	data, err := os.ReadFile(filepath.Join(first.Record.WorktreePath, "tracked.txt"))
	if err != nil || string(data) != "committed\n" {
		t.Fatalf("worktree tracked file=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(first.Record.WorktreePath, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked source file was copied: %v", err)
	}

	// A new Manager simulates a Runtime restart; the same idempotency key must
	// load the existing ready session without creating another checkout.
	afterRestart, err := New(store, repo).Create(context.Background(), req)
	if err != nil || afterRestart.Session.ID != first.Session.ID {
		t.Fatalf("restart result=%+v err=%v", afterRestart, err)
	}
	worktrees := git(t, root, "worktree", "list", "--porcelain")
	if got := strings.Count(worktrees, "worktree "); got != 2 {
		t.Fatalf("git worktree count=%d output=%s", got, worktrees)
	}
}

func TestCreateManagedWorktreeRejectsInvalidBaseRef(t *testing.T) {
	root := initGitRepo(t)
	_, err := New(session.NewMemoryStore(), newMemoryRepo()).Create(context.Background(), CreateRequest{
		ClientRequestID: "bad", SourceWorkspacePath: root, BaseRef: "working-tree",
	})
	var managedErr *Error
	if !errors.As(err, &managedErr) || managedErr.Code != CodeBaseRefUnavailable {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".codeagent")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid request created .codeagent: %v", statErr)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "tracked.txt")
	git(t, root, "commit", "-m", "initial")
	return root
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
