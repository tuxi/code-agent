package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitInit_FreshDir(t *testing.T) {
	dir := t.TempDir()
	out := runTool(t, NewGitInitTool(), dir, struct{}{})
	if !strings.Contains(out, "Initialized empty git repository") {
		t.Fatalf("init output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf(".git not created: %v", err)
	}
}

func TestGitInit_AlreadyRepo(t *testing.T) {
	dir := initTestRepo(t)
	out := runTool(t, NewGitInitTool(), dir, struct{}{})
	if !strings.Contains(out, "Already a git repository") {
		t.Errorf("re-init output = %q, want 'Already a git repository'", out)
	}
}

// The keystone path: a plain folder can be init'd and immediately committed to,
// entirely in pure Go (no git binary) — the iOS flow.
func TestGitInit_ThenCommit(t *testing.T) {
	dir := t.TempDir()

	if out := runTool(t, NewGitInitTool(), dir, struct{}{}); !strings.Contains(out, "Initialized") {
		t.Fatalf("init failed: %s", out)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	commit := NewGitCommitToolGoGit()
	input, _ := json.Marshal(gitCommitInput{Message: "initial", All: true})
	res, err := commit.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	var result gitCommitResult
	_ = json.Unmarshal([]byte(res.Content), &result)
	if result.ExitCode != 0 || result.Hash == "" {
		t.Fatalf("commit after init failed: exit=%d stderr=%q", result.ExitCode, result.Stderr)
	}

	// And log now shows it.
	logOut := runTool(t, NewGitLogTool(), dir, gitLogInput{})
	if !strings.Contains(logOut, "initial") {
		t.Errorf("log missing the commit:\n%s", logOut)
	}
}
