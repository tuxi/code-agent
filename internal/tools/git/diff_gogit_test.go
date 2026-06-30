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

// runGoGitDiff is a small helper: it runs the go-git diff tool and returns the
// content string.
func runGoGitDiff(t *testing.T, dir string, in diffInput) string {
	t.Helper()
	tool := NewDiffToolGoGit()
	input, _ := json.Marshal(in)
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Content
}

func TestGoGitDiff_ModifiedFile(t *testing.T) {
	dir := initTestRepo(t) // has README.md committed with "# test\n"

	// Modify a committed file.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runGoGitDiff(t, dir, diffInput{})
	if !strings.Contains(out, "diff --git a/README.md b/README.md") {
		t.Errorf("missing git header:\n%s", out)
	}
	if !strings.Contains(out, "@@") {
		t.Errorf("missing unified hunk header:\n%s", out)
	}
	if !strings.Contains(out, "+more") {
		t.Errorf("missing added line:\n%s", out)
	}
}

func TestGoGitDiff_CleanTree(t *testing.T) {
	dir := initTestRepo(t)
	if out := runGoGitDiff(t, dir, diffInput{}); out != "No git diff." {
		t.Errorf("clean tree diff = %q, want 'No git diff.'", out)
	}
}

func TestGoGitDiff_UntrackedOmitted(t *testing.T) {
	dir := initTestRepo(t)
	// A brand-new, unstaged file is untracked → git diff omits it.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runGoGitDiff(t, dir, diffInput{}); out != "No git diff." {
		t.Errorf("untracked file should be omitted, got:\n%s", out)
	}
}

func TestGoGitDiff_DeletedFile(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	out := runGoGitDiff(t, dir, diffInput{})
	if !strings.Contains(out, "a/README.md") || !strings.Contains(out, "-# test") {
		t.Errorf("deletion not reflected:\n%s", out)
	}
}

func TestGoGitDiff_PathFilter(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteCommitted(t, dir, "other.txt", "a\n")
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Filtering to other.txt should exclude README.md.
	out := runGoGitDiff(t, dir, diffInput{Path: "other.txt"})
	if strings.Contains(out, "README.md") {
		t.Errorf("path filter leaked README.md:\n%s", out)
	}
	if !strings.Contains(out, "other.txt") {
		t.Errorf("path filter dropped target:\n%s", out)
	}
}

func TestGoGitDiff_Stat(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\nadded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runGoGitDiff(t, dir, diffInput{Stat: true})
	if !strings.Contains(out, "README.md") || !strings.Contains(out, "+") {
		t.Errorf("stat output unexpected:\n%s", out)
	}
	if strings.Contains(out, "@@") {
		t.Errorf("stat should not include hunks:\n%s", out)
	}
}

func TestGoGitDiff_Binary(t *testing.T) {
	dir := initTestRepo(t)
	// Commit a binary file, then change it.
	mustWriteCommitted(t, dir, "blob.bin", "\x00\x01\x02")
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("\x00\x09\x09"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runGoGitDiff(t, dir, diffInput{})
	if !strings.Contains(out, "Binary files") {
		t.Errorf("binary change not reported as binary:\n%s", out)
	}
}

// mustWriteCommitted writes a file and commits it via the go-git commit tool, so
// the fixture setup itself does not depend on the git binary beyond initTestRepo.
func mustWriteCommitted(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewGitCommitToolGoGit()
	input, _ := json.Marshal(gitCommitInput{Message: "add " + name, All: true})
	if _, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input); err != nil {
		t.Fatalf("commit fixture %s: %v", name, err)
	}
}
