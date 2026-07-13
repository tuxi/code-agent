package managedworktree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/session"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	gittools "code-agent/internal/tools/git"
	"code-agent/internal/tools/search"
	"code-agent/internal/worktree"
)

func TestManagedWorktreeExcludeAndWorkspaceDiscoveryIsolation(t *testing.T) {
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	repo := newMemoryRepo()
	manager := New(store, repo)
	first, err := manager.Create(context.Background(), CreateRequest{
		ClientRequestID: "isolation-1", SourceWorkspacePath: root,
		SuggestedName: "hidden", BaseRef: worktree.BaseRefHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	marker := "MANAGED_WORKTREE_SECRET_MARKER"
	if err := os.WriteFile(filepath.Join(first.Record.WorktreePath, "private.txt"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), CreateRequest{
		ClientRequestID: "isolation-2", SourceWorkspacePath: root,
		SuggestedName: "other", BaseRef: worktree.BaseRefHead,
	}); err != nil {
		t.Fatal(err)
	}

	common := strings.TrimSpace(git(t, root, "rev-parse", "--git-common-dir"))
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	exclude, err := os.ReadFile(filepath.Join(common, "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(exclude), "/.codeagent/worktrees/"); count != 1 {
		t.Fatalf("exclude count=%d content=%q", count, exclude)
	}

	ec := tools.ExecutionContext{WorkspaceRoot: root, TurnID: "turn", CallID: "call"}
	list := filesystem.NewListFilesTool()
	list.MaxDepth = 10
	listed, err := list.Execute(context.Background(), ec, json.RawMessage(`{"path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listed.Content, ".codeagent/worktrees") || strings.Contains(listed.Content, "private.txt") {
		t.Fatalf("base list exposed managed worktree: %s", listed.Content)
	}
	grep := search.NewGrepTool()
	searched, err := grep.Execute(context.Background(), ec, json.RawMessage(`{"query":"MANAGED_WORKTREE_SECRET_MARKER","path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	if searched.Content != "No matches." {
		t.Fatalf("base grep exposed managed worktree: %s", searched.Content)
	}
	status := git(t, root, "status", "--porcelain", "--untracked-files=normal")
	if strings.Contains(status, ".codeagent") {
		t.Fatalf("Git status exposed managed root: %s", status)
	}
	statusResult, err := gittools.NewGitStatusTool().Execute(context.Background(), ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(statusResult.Content, ".codeagent") || strings.Contains(statusResult.Content, "private.txt") {
		t.Fatalf("go-git status exposed managed root: %s", statusResult.Content)
	}
}

func TestManagedWorktreeRejectsNestedSource(t *testing.T) {
	root := initGitRepo(t)
	store := session.NewMemoryStore()
	repo := newMemoryRepo()
	manager := New(store, repo)
	first, err := manager.Create(context.Background(), CreateRequest{
		ClientRequestID: "parent", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Create(context.Background(), CreateRequest{
		ClientRequestID: "nested", SourceWorkspacePath: first.Record.WorktreePath, BaseRef: worktree.BaseRefHead,
	})
	var managedErr *Error
	if !errors.As(err, &managedErr) || managedErr.Code != CodeNestedNotAllowed {
		t.Fatalf("nested err=%v", err)
	}
}

func TestManagedWorktreeRejectsSymlinkedContainerEscape(t *testing.T) {
	root := initGitRepo(t)
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".codeagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".codeagent", "worktrees")); err != nil {
		t.Fatal(err)
	}
	_, err := New(session.NewMemoryStore(), newMemoryRepo()).Create(context.Background(), CreateRequest{
		ClientRequestID: "escape", SourceWorkspacePath: root, BaseRef: worktree.BaseRefHead,
	})
	var managedErr *Error
	if !errors.As(err, &managedErr) || managedErr.Code != CodeEscapeDetected {
		t.Fatalf("escape err=%v", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("escape target was modified: entries=%v err=%v", entries, readErr)
	}
}
