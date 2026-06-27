package git

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a temporary git repo with an initial commit and returns
// its path. The caller is responsible for cleaning up (t.TempDir() handles it).
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init")
	run("config", "user.name", "test")
	run("config", "user.email", "test@test.com")

	// Create an initial file and commit so the repo has a HEAD.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial commit")

	return dir
}

func TestGitCommitBasic(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitTool()

	// Make a change.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stage it.
	cmd := exec.Command("git", "add", "hello.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	input, _ := json.Marshal(gitCommitInput{Message: "add hello.txt"})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result gitCommitResult
	if err := json.Unmarshal([]byte(res.Content), &result); err != nil {
		t.Fatalf("parse result: %v\n%s", err, res.Content)
	}

	if result.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (stderr=%q)", result.ExitCode, result.Stderr)
	}
	if result.Hash == "" {
		t.Error("hash is empty, expected a commit SHA")
	}
	if result.ShortHash == "" {
		t.Error("short_hash is empty")
	}
	if result.Subject != "add hello.txt" {
		t.Errorf("subject = %q, want %q", result.Subject, "add hello.txt")
	}
}

func TestGitCommitMultiLineMessage(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitTool()

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "feature.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	// Multi-line message with special characters that would break shell quoting.
	msg := "feat: add \"cool\" feature\n\nThis commit's message has:\n- double quotes \"like this\"\n- single quotes 'like this'\n- backticks `like this`\n- a pipe | and ampersand &\n"
	input, _ := json.Marshal(gitCommitInput{Message: msg})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result gitCommitResult
	if err := json.Unmarshal([]byte(res.Content), &result); err != nil {
		t.Fatalf("parse result: %v\n%s", err, res.Content)
	}

	if result.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (stderr=%q)", result.ExitCode, result.Stderr)
	}
	if result.Subject != "feat: add \"cool\" feature" {
		t.Errorf("subject = %q, want %q", result.Subject, "feat: add \"cool\" feature")
	}

	// Verify the full message was stored correctly by reading it back from git.
	cmd = exec.Command("git", "log", "-1", "--format=%B")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	gotMsg := strings.TrimSpace(string(out))
	wantMsg := strings.TrimSpace(msg)
	if gotMsg != wantMsg {
		t.Errorf("stored message mismatch:\ngot:\n%s\nwant:\n%s", gotMsg, wantMsg)
	}
}

func TestGitCommitEmptyMessage(t *testing.T) {
	dir := t.TempDir()
	tool := NewGitCommitTool()
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, json.RawMessage(`{"message":""}`))
	if err == nil {
		t.Error("expected error for empty message")
	}
}

func TestGitCommitNothingToCommit(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitTool()

	input, _ := json.Marshal(gitCommitInput{Message: "should fail"})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result gitCommitResult
	if err := json.Unmarshal([]byte(res.Content), &result); err != nil {
		t.Fatalf("parse result: %v\n%s", err, res.Content)
	}

	if result.ExitCode == 0 {
		t.Error("expected non-zero exit code when nothing to commit")
	}
}

func TestGitCommitAllUntrackedFile(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitTool()

	// Create a new file WITHOUT staging it — this is the whole point.
	if err := os.WriteFile(filepath.Join(dir, "new_file.txt"), []byte("untracked content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(gitCommitInput{Message: "commit untracked", All: true})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result gitCommitResult
	if err := json.Unmarshal([]byte(res.Content), &result); err != nil {
		t.Fatalf("parse result: %v\n%s", err, res.Content)
	}

	if result.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0 (stderr=%q)", result.ExitCode, result.Stderr)
	}
	if result.Hash == "" {
		t.Error("hash is empty, expected a commit SHA")
	}

	// Verify new_file.txt IS in the commit.
	cmd := exec.Command("git", "show", "--name-only", "--format=", result.Hash)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git show: %v\n%s", err, out)
	}
	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	found := false
	for _, f := range files {
		if f == "new_file.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("new_file.txt not found in commit; committed files: %v", files)
	}
}

func TestGitCommitAllStagedFilesInResult(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitTool()

	if err := os.WriteFile(filepath.Join(dir, "staged_test.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(gitCommitInput{Message: "with staged", All: true})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result gitCommitResult
	if err := json.Unmarshal([]byte(res.Content), &result); err != nil {
		t.Fatalf("parse result: %v\n%s", err, res.Content)
	}

	if result.Staged == "" {
		t.Error("staged is empty; expected git status --short output")
	}
	if !strings.Contains(result.Staged, "staged_test.txt") {
		t.Errorf("staged should mention staged_test.txt, got: %q", result.Staged)
	}
}

func TestGitCommitAllRespectsGitignore(t *testing.T) {
	dir := initTestRepo(t)
	tool := NewGitCommitTool()

	// Create .gitignore and a file it should exclude.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "debug.log"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(gitCommitInput{Message: "respects gitignore", All: true})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result gitCommitResult
	if err := json.Unmarshal([]byte(res.Content), &result); err != nil {
		t.Fatalf("parse result: %v\n%s", err, res.Content)
	}

	if result.ExitCode != 0 {
		// .gitignore itself is a new untracked file that IS staged, so the commit
		// should succeed.
		t.Errorf("exit_code = %d, want 0 (stderr=%q, staged=%q)", result.ExitCode, result.Stderr, result.Staged)
	}
	if result.Hash == "" {
		t.Fatal("hash is empty, expected a commit SHA")
	}

	// Verify debug.log is NOT in the commit.
	cmd := exec.Command("git", "show", "--name-only", "--format=", result.Hash)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git show: %v\n%s", err, out)
	}
	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, f := range files {
		if f == "debug.log" {
			t.Errorf("debug.log should NOT be committed (it matches .gitignore)")
		}
	}

	// Verify .gitignore IS in the commit.
	found := false
	for _, f := range files {
		if f == ".gitignore" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf(".gitignore should be committed (it's not ignored)")
	}
}
