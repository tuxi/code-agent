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
