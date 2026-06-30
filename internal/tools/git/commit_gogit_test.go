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

// The go-git committer must produce the same observable behavior as the exec one,
// since on iOS it is the only backend. These tests reuse initTestRepo (which needs
// the git binary to *create* the fixture) but exercise the pure-Go commit path.

func TestGoGitCommit_StagedFile(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitToolGoGit()

	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// all=true stages the new file and commits it in one go-git pass.
	input, _ := json.Marshal(gitCommitInput{Message: "add hello.txt", All: true})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result gitCommitResult
	if err := json.Unmarshal([]byte(res.Content), &result); err != nil {
		t.Fatalf("parse: %v\n%s", err, res.Content)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 (stderr=%q)", result.ExitCode, result.Stderr)
	}
	if len(result.Hash) != 40 {
		t.Errorf("hash = %q, want a 40-char SHA", result.Hash)
	}
	if result.Subject != "add hello.txt" {
		t.Errorf("subject = %q", result.Subject)
	}
	if !strings.Contains(result.Staged, "hello.txt") {
		t.Errorf("staged = %q, want it to mention hello.txt", result.Staged)
	}
}

func TestGoGitCommit_NothingToCommit(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitToolGoGit()

	input, _ := json.Marshal(gitCommitInput{Message: "noop"})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result gitCommitResult
	_ = json.Unmarshal([]byte(res.Content), &result)
	if result.ExitCode == 0 {
		t.Errorf("exit_code = 0, want non-zero for an empty commit")
	}
	if result.Hash != "" {
		t.Errorf("hash = %q, want empty when nothing was committed", result.Hash)
	}
}

func TestGoGitCommit_AllRespectsGitignore(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitToolGoGit()

	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(gitCommitInput{Message: "add kept", All: true})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result gitCommitResult
	_ = json.Unmarshal([]byte(res.Content), &result)
	if strings.Contains(result.Staged, "ignored.txt") {
		t.Errorf("staged includes a gitignored file: %q", result.Staged)
	}
	if !strings.Contains(result.Staged, "kept.txt") {
		t.Errorf("staged should include kept.txt: %q", result.Staged)
	}
}
