package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// gitInitEmpty creates a git repository with no commits.
func gitInitEmpty(dir string) (*gogit.Repository, error) {
	return gogit.PlainInit(dir, false)
}

func runTool(t *testing.T, tool tools.Tool, dir string, in any) string {
	t.Helper()
	input, _ := json.Marshal(in)
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("%s Execute: %v", tool.Name(), err)
	}
	return res.Content
}

func TestGitStatus_CleanAndDirty(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitStatusTool()

	clean := runTool(t, tool, dir, struct{}{})
	if !strings.Contains(clean, "On branch") {
		t.Errorf("status missing branch line:\n%s", clean)
	}
	if !strings.Contains(clean, "working tree clean") {
		t.Errorf("fresh repo should be clean:\n%s", clean)
	}

	// Introduce an untracked file and a modification.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty := runTool(t, tool, dir, struct{}{})
	if strings.Contains(dirty, "working tree clean") {
		t.Errorf("dirty tree reported clean:\n%s", dirty)
	}
	if !strings.Contains(dirty, "README.md") || !strings.Contains(dirty, "untracked.txt") {
		t.Errorf("status missing changed files:\n%s", dirty)
	}
}

func TestGitStatus_NotARepo(t *testing.T) {
	dir := t.TempDir()
	out := runTool(t, NewGitStatusTool(), dir, struct{}{})
	if !strings.Contains(out, "Not a git repository") {
		t.Errorf("got %q", out)
	}
}

func TestGitLog_RecentCommits(t *testing.T) {
	dir := initTestRepo(t) // one "initial commit"
	// Add two more commits via the go-git commit tool.
	mustWriteCommitted(t, dir, "a.txt", "a\n")
	mustWriteCommitted(t, dir, "b.txt", "b\n")

	out := runTool(t, NewGitLogTool(), dir, gitLogInput{Limit: 10})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 commits, got %d:\n%s", len(lines), out)
	}
	// Most recent first: "add b.txt" before "add a.txt" before "initial commit".
	if !strings.Contains(lines[0], "add b.txt") {
		t.Errorf("first line not most-recent commit: %q", lines[0])
	}
	if !strings.Contains(lines[2], "initial commit") {
		t.Errorf("last line not oldest commit: %q", lines[2])
	}
}

func TestGitLog_LimitAndPath(t *testing.T) {
	dir := initTestRepo(t)
	mustWriteCommitted(t, dir, "only.txt", "1\n")
	mustWriteCommitted(t, dir, "other.txt", "2\n")

	// limit=1 → a single line.
	out := runTool(t, NewGitLogTool(), dir, gitLogInput{Limit: 1})
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n != 1 {
		t.Errorf("limit=1 returned %d lines:\n%s", n, out)
	}

	// path filter → only commits touching only.txt.
	out = runTool(t, NewGitLogTool(), dir, gitLogInput{Path: "only.txt"})
	if !strings.Contains(out, "add only.txt") {
		t.Errorf("path-filtered log missing target commit:\n%s", out)
	}
	if strings.Contains(out, "add other.txt") {
		t.Errorf("path-filtered log leaked unrelated commit:\n%s", out)
	}
}

func TestGitLog_NoCommits(t *testing.T) {
	dir := t.TempDir()
	// init a bare-ish repo with no commits via go-git.
	if _, err := gitInitEmpty(dir); err != nil {
		t.Skip("cannot init empty repo:", err)
	}
	out := runTool(t, NewGitLogTool(), dir, gitLogInput{})
	if !strings.Contains(out, "No commits") {
		t.Errorf("got %q", out)
	}
}
