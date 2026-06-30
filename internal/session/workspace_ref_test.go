package session

import (
	"os"
	"path/filepath"
	"testing"
)

// §11.1 + §11.6: a workspace-rooted ref re-anchors under a *different* workspaceDir
// (simulating an iOS reinstall where the container <UUID> changed), and the
// relativize→resolve round-trip is stable under /private-prefix / symlink normalization.
func TestWorkspaceRef_ReanchorAcrossWorkspaceDir(t *testing.T) {
	oldBase := t.TempDir() // stands in for the old container's Documents
	newBase := t.TempDir() // the new container's Documents this launch
	mustMkdir(t, filepath.Join(oldBase, "MyProj", "sub"))
	mustMkdir(t, filepath.Join(newBase, "MyProj", "sub"))

	proj := filepath.Join(oldBase, "MyProj", "sub")
	ref := ToWorkspaceRef(proj, oldBase, "")
	if ref.Root != RootWorkspace {
		t.Fatalf("Root = %q, want %q", ref.Root, RootWorkspace)
	}
	if ref.Rel != filepath.Join("MyProj", "sub") {
		t.Fatalf("Rel = %q, want %q", ref.Rel, filepath.Join("MyProj", "sub"))
	}

	got, err := ref.Resolve(newBase, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := normalizePath(filepath.Join(newBase, "MyProj", "sub"))
	if got != want {
		t.Fatalf("re-anchored = %q, want %q", got, want)
	}
}

// rel == "." when the project IS the workspaceDir; safeJoin(base, ".") == base.
func TestWorkspaceRef_RootItself(t *testing.T) {
	base := t.TempDir()
	ref := ToWorkspaceRef(base, base, "")
	if ref.Root != RootWorkspace || ref.Rel != "." {
		t.Fatalf("got Root=%q Rel=%q, want workspace/.", ref.Root, ref.Rel)
	}
	got, err := ref.Resolve(base, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != normalizePath(base) {
		t.Fatalf("got %q, want %q", got, normalizePath(base))
	}
}

// §11.2: an external ref with no host-supplied path and a non-absolute ext_id needs
// host rebind; supplying a fresh absolute path resolves it.
func TestWorkspaceRef_ExternalNeedsRebind(t *testing.T) {
	ref := WorkspaceRef{Root: RootExternal, ExtID: "BKMK-7f3a"} // bookmark id, not a path
	if _, err := ref.Resolve("/any", ""); err != ErrExternalNeedsHostRebind {
		t.Fatalf("err = %v, want ErrExternalNeedsHostRebind", err)
	}
	got, err := ref.Resolve("/any", "/var/new/MyProj")
	if err != nil {
		t.Fatalf("Resolve with host abs: %v", err)
	}
	if got != normalizePath("/var/new/MyProj") {
		t.Fatalf("got %q", got)
	}
}

// §11.5 (mac): empty workspaceDir → external anchored on the absolute path, which is
// stable, so Resolve returns it without any host involvement (old behavior preserved).
func TestWorkspaceRef_MacAbsoluteStable(t *testing.T) {
	abs := "/Users/me/projects/app"
	ref := ToWorkspaceRef(abs, "", "")
	if ref.Root != RootExternal {
		t.Fatalf("Root = %q, want external", ref.Root)
	}
	got, err := ref.Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != normalizePath(abs) {
		t.Fatalf("got %q, want %q", got, normalizePath(abs))
	}
}

// §11.3: a rel that escapes its base via ".." is rejected by safeJoin (defense
// against tampered/legacy data), surfaced through Resolve.
func TestWorkspaceRef_RejectsEscape(t *testing.T) {
	base := t.TempDir()
	ref := WorkspaceRef{Root: RootWorkspace, Rel: filepath.Join("..", "..", "etc")}
	if _, err := ref.Resolve(base, ""); err == nil {
		t.Fatal("expected safeJoin to reject a '..' escape, got nil")
	}
}

// §11.4: legacy absolute path is salvaged by its tail after /Documents/ when that
// tail exists under the current workspaceDir; otherwise it degrades to external
// without losing data.
func TestMigrateLegacyWorkspacePath(t *testing.T) {
	newBase := t.TempDir()
	mustMkdir(t, filepath.Join(newBase, "MyProj"))

	// Old absolute path with a dead container <UUID> but a recognizable tail.
	legacy := "/private/var/mobile/Containers/Data/Application/OLD-UUID/Documents/MyProj"
	ref := MigrateLegacyWorkspacePath(legacy, newBase)
	if ref.Root != RootWorkspace || ref.Rel != "MyProj" {
		t.Fatalf("salvage: Root=%q Rel=%q, want workspace/MyProj", ref.Root, ref.Rel)
	}

	// Tail does not exist under the new base → external, data retained as ext_id.
	legacyGone := "/private/var/mobile/Containers/Data/Application/OLD-UUID/Documents/Vanished"
	ref2 := MigrateLegacyWorkspacePath(legacyGone, newBase)
	if ref2.Root != RootExternal || ref2.ExtID == "" {
		t.Fatalf("unsalvageable: Root=%q ExtID=%q, want external with abs ext_id", ref2.Root, ref2.ExtID)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}
