package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

func TestNormalizeURL(t *testing.T) {
	ok := map[string]string{
		"https://github.com/owner/repo":     "https://github.com/owner/repo",
		"https://github.com/owner/repo.git": "https://github.com/owner/repo.git",
		"owner/repo":                        "https://github.com/owner/repo",
		"owner/repo.git":                    "https://github.com/owner/repo",
	}
	for in, want := range ok {
		got, cerr := normalizeURL(in)
		if cerr != nil {
			t.Errorf("normalizeURL(%q) error: %v", in, cerr)
			continue
		}
		if got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{"", "http://github.com/o/r", "https://gitlab.com/o/r", "ssh://git@github.com/o/r", "just-one-word"}
	for _, in := range bad {
		if _, cerr := normalizeURL(in); cerr == nil {
			t.Errorf("normalizeURL(%q) should have failed", in)
		} else if cerr.Code != "invalid_url" {
			t.Errorf("normalizeURL(%q) code = %q, want invalid_url", in, cerr.Code)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"repo", "my-repo", "repo123"} {
		if !validName(n) {
			t.Errorf("validName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", ".", "..", "a/b", "../escape", "/abs", `a\b`} {
		if validName(n) {
			t.Errorf("validName(%q) = true, want false", n)
		}
	}
}

// makeLocalRepo creates a real git repo on disk to serve as a clone source.
func makeLocalRepo(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	repo, err := gogit.PlainInit(src, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &gogit.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return src
}

// cloneLocal bypasses normalizeURL's GitHub-only check by calling go-git directly
// the way Clone does, so we can exercise target/conflict logic against a file://
// source. We test URL normalization separately above.
func TestClone_TargetConfinementAndConflict(t *testing.T) {
	src := makeLocalRepo(t)
	ws := t.TempDir()
	ctx := context.Background()

	// Use file:// + skip the github check by invoking the lower-level pieces. Since
	// Clone enforces github.com, we test the unique-target + clone mechanics by
	// cloning twice into the same base name via a small local helper.
	cloneOnce := func(name string) (*CloneResult, error) {
		rel, abs := uniqueTarget(ws, name)
		if !isUnder(abs, ws) {
			t.Fatalf("target escaped workspace: %s", abs)
		}
		_, err := gogit.PlainCloneContext(ctx, abs, false, &gogit.CloneOptions{URL: "file://" + src, Depth: 1})
		if err != nil {
			return nil, err
		}
		return &CloneResult{AbsPath: abs, Rel: rel}, nil
	}

	r1, err := cloneOnce("repo")
	if err != nil {
		t.Fatalf("first clone: %v", err)
	}
	if r1.Rel != "repo" {
		t.Errorf("first rel = %q, want repo", r1.Rel)
	}
	if _, err := os.Stat(filepath.Join(r1.AbsPath, "README.md")); err != nil {
		t.Errorf("cloned file missing: %v", err)
	}

	// Second clone of same base name must auto-increment, not overwrite.
	r2, err := cloneOnce("repo")
	if err != nil {
		t.Fatalf("second clone: %v", err)
	}
	if r2.Rel != "repo-2" {
		t.Errorf("second rel = %q, want repo-2", r2.Rel)
	}
}

func TestClone_InvalidName(t *testing.T) {
	ws := t.TempDir()
	_, err := Clone(context.Background(), ws, CloneOptions{URL: "owner/repo", Name: "../escape"})
	ce, ok := err.(*CloneError)
	if !ok || ce.Code != "invalid_name" {
		t.Fatalf("err = %v, want invalid_name CloneError", err)
	}
}

func TestClone_InvalidURL(t *testing.T) {
	ws := t.TempDir()
	_, err := Clone(context.Background(), ws, CloneOptions{URL: "https://gitlab.com/o/r"})
	ce, ok := err.(*CloneError)
	if !ok || ce.Code != "invalid_url" {
		t.Fatalf("err = %v, want invalid_url CloneError", err)
	}
}

func TestUniqueTarget(t *testing.T) {
	ws := t.TempDir()
	// First is free.
	rel, _ := uniqueTarget(ws, "x")
	if rel != "x" {
		t.Errorf("rel = %q, want x", rel)
	}
	// Occupy x and x-2 with non-empty dirs.
	for _, n := range []string{"x", "x-2"} {
		d := filepath.Join(ws, n)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "f"), []byte("y"), 0o644)
	}
	rel, _ = uniqueTarget(ws, "x")
	if rel != "x-3" {
		t.Errorf("rel = %q, want x-3", rel)
	}
}
