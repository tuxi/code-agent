package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedWorktreeBoundaryBlocksDirectCaseVariantAndSymlink(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, ".codeagent", "worktrees", "one")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(managed, "secret.txt")
	if err := os.WriteFile(file, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		file,
		filepath.Join(root, ".CODEAGENT", "WORKTREES", "one", "secret.txt"),
	} {
		if err := ValidatePath(root, target); !errors.Is(err, ErrManagedWorktreeBoundary) {
			t.Fatalf("ValidatePath(%q)=%v", target, err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(managed, alias); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(root, filepath.Join(alias, "secret.txt")); !errors.Is(err, ErrManagedWorktreeBoundary) {
		t.Fatalf("symlink boundary err=%v", err)
	}
}

func TestClassifyPath(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "readme.md")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("inside workspace", func(t *testing.T) {
		if got := ClassifyPath(root, file); got != PathInsideWorkspace {
			t.Fatalf("ClassifyPath(%q, %q) = %v, want PathInsideWorkspace", root, file, got)
		}
	})

	t.Run("outside workspace", func(t *testing.T) {
		external := filepath.Join(t.TempDir(), "other.txt")
		if err := os.WriteFile(external, []byte("ext"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ClassifyPath(root, external); got != PathOutsideWorkspace {
			t.Fatalf("ClassifyPath(%q, %q) = %v, want PathOutsideWorkspace", root, external, got)
		}
	})

	t.Run("outside workspace via parent traversal", func(t *testing.T) {
		parent := filepath.Join(root, "../outside.txt")
		if got := ClassifyPath(root, parent); got != PathOutsideWorkspace {
			t.Fatalf("ClassifyPath(%q, %q) = %v, want PathOutsideWorkspace", root, parent, got)
		}
	})

	t.Run("managed worktree", func(t *testing.T) {
		managed := filepath.Join(root, ".codeagent", "worktrees", "one")
		if err := os.MkdirAll(managed, 0o755); err != nil {
			t.Fatal(err)
		}
		secret := filepath.Join(managed, "secret.txt")
		if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ClassifyPath(root, secret); got != PathManagedWorktree {
			t.Fatalf("ClassifyPath(%q, %q) = %v, want PathManagedWorktree", root, secret, got)
		}
	})

	t.Run("managed worktree via symlink", func(t *testing.T) {
		managed := filepath.Join(root, ".codeagent", "worktrees", "sym")
		if err := os.MkdirAll(managed, 0o755); err != nil {
			t.Fatal(err)
		}
		secret := filepath.Join(managed, "secret.txt")
		if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "alias")
		if err := os.Symlink(managed, alias); err != nil {
			t.Fatal(err)
		}
		if got := ClassifyPath(root, filepath.Join(alias, "secret.txt")); got != PathManagedWorktree {
			t.Fatalf("symlink to managed worktree = %v, want PathManagedWorktree", got)
		}
	})

	t.Run("nonexistent external path", func(t *testing.T) {
		external := filepath.Join(root, "..", "does-not-exist", "file.txt")
		if got := ClassifyPath(root, external); got != PathOutsideWorkspace {
			t.Fatalf("ClassifyPath(%q, %q) = %v, want PathOutsideWorkspace", root, external, got)
		}
	})
}

func TestClassifyPathValidatesExistingTests(t *testing.T) {
	// Verify that ValidatePath and ClassifyPath agree on all existing test cases.
	root := t.TempDir()

	// Sub-test: inside workspace → PathInsideWorkspace → ValidatePath returns nil
	t.Run("inside agrees", func(t *testing.T) {
		file := filepath.Join(root, "readme.md")
		if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ClassifyPath(root, file) != PathInsideWorkspace {
			t.Fatal("ClassifyPath should return PathInsideWorkspace")
		}
		if err := ValidatePath(root, file); err != nil {
			t.Fatalf("ValidatePath should pass for inside-workspace path: %v", err)
		}
	})

	// Sub-test: managed worktree → PathManagedWorktree → ValidatePath returns ErrManagedWorktreeBoundary
	t.Run("managed agrees", func(t *testing.T) {
		managed := filepath.Join(root, ".codeagent", "worktrees", "one")
		if err := os.MkdirAll(managed, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(managed, "secret.txt")
		if err := os.WriteFile(file, []byte("secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ClassifyPath(root, file) != PathManagedWorktree {
			t.Fatal("ClassifyPath should return PathManagedWorktree")
		}
		if err := ValidatePath(root, file); !errors.Is(err, ErrManagedWorktreeBoundary) {
			t.Fatalf("ValidatePath should return ErrManagedWorktreeBoundary: %v", err)
		}
	})
}

func TestManagedCheckoutCanAccessItsOwnFiles(t *testing.T) {
	base := t.TempDir()
	managed := filepath.Join(base, ".codeagent", "worktrees", "one")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(managed, "source.go")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(managed, file); err != nil {
		t.Fatalf("managed checkout blocked its own file: %v", err)
	}
}

func TestResolveToolPath(t *testing.T) {
	root := "/Users/test/my-workspace"

	t.Run("relative path joins with root", func(t *testing.T) {
		got := ResolveToolPath(root, "src/main.go")
		want := filepath.Join(root, "src/main.go")
		if got != want {
			t.Fatalf("ResolveToolPath(%q, %q) = %q, want %q", root, "src/main.go", got, want)
		}
	})

	t.Run("absolute path is used directly", func(t *testing.T) {
		got := ResolveToolPath(root, "/Users/other/project/README.md")
		want := "/Users/other/project/README.md"
		if got != want {
			t.Fatalf("ResolveToolPath(%q, %q) = %q, want %q", root, "/Users/other/project/README.md", got, want)
		}
	})

	t.Run("absolute path with .. traversal is cleaned", func(t *testing.T) {
		got := ResolveToolPath(root, "/Users/other/../project/README.md")
		want := "/Users/project/README.md"
		if got != want {
			t.Fatalf("ResolveToolPath(%q, %q) = %q, want %q", root, "/Users/other/../project/README.md", got, want)
		}
	})

	t.Run("dot path resolves to root", func(t *testing.T) {
		got := ResolveToolPath(root, ".")
		want := root
		if got != want {
			t.Fatalf("ResolveToolPath(%q, %q) = %q, want %q", root, ".", got, want)
		}
	})

	t.Run("empty path resolves to root", func(t *testing.T) {
		got := ResolveToolPath(root, "")
		want := root
		if got != want {
			t.Fatalf("ResolveToolPath(%q, %q) = %q, want %q", root, "", got, want)
		}
	})
}
