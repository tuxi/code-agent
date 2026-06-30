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

func TestRepoNameFromURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://github.com/user/repo.git", "repo"},
		{"https://github.com/user/repo", "repo"},
		{"https://example.com/r", "r"},
		{"https://example.com/path/to/x.git", "x"},
	}
	for _, c := range cases {
		if got := repoNameFromURL(c.url); got != c.want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestGitClone_RejectsNonHTTPS(t *testing.T) {
	dir := t.TempDir()
	tool := NewGitCloneTool()
	in, _ := json.Marshal(gitCloneInput{URL: "ssh://git@github.com:user/repo.git"})
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, in)
	if err == nil || !strings.Contains(err.Error(), "only https://") {
		t.Errorf("should reject ssh URL, got err=%v", err)
	}
}

func TestGitClone_LocalRepo(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Set up a fake remote: init a repo, commit a file, serve it from disk.
	srcRepo, err := gogit.PlainInit(src, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := srcRepo.Worktree()
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &gogit.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	tool := NewGitCloneTool()
	// Clone via the tool into the dst workspace.
	out := runTool(t, tool, dst, gitCloneInput{URL: "file://" + src})
	if !strings.Contains(out, "Cloned") {
		t.Fatalf("clone failed: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dst, repoNameFromURL(src), "README.md")); err != nil {
		t.Errorf("cloned file not found: %v", err)
	}
}

func TestGitClone_EmptyURL(t *testing.T) {
	dir := t.TempDir()
	tool := NewGitCloneTool()
	in, _ := json.Marshal(gitCloneInput{})
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, in)
	if err == nil || !strings.Contains(err.Error(), "url is required") {
		t.Errorf("got err=%v, want 'url is required'", err)
	}
}
