package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePathIsHomeBasedAndProjectScoped(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	a, err := storePath("/Users/x/Documents/work/learning-golang")
	if err != nil {
		t.Fatal(err)
	}
	// Lives under ~/.codeagent, never inside the (possibly synced) project dir.
	if !strings.HasPrefix(a, filepath.Join(home, ".codeagent")) {
		t.Errorf("store path %q is not under ~/.codeagent", a)
	}
	if strings.Contains(a, "/Documents/") {
		t.Errorf("store path %q must not be inside the project (synced) dir", a)
	}
	// Project-scoped: embeds the basename.
	if !strings.Contains(a, "learning-golang-") {
		t.Errorf("store path %q should embed the project basename", a)
	}
	// Stable for the same root.
	if a2, _ := storePath("/Users/x/Documents/work/learning-golang"); a != a2 {
		t.Error("storePath should be stable for the same root")
	}
	// Same basename, different parent → different DB (no collision).
	if b, _ := storePath("/Users/x/other/learning-golang"); a == b {
		t.Error("distinct project paths must map to distinct DBs")
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.db")
	dst := filepath.Join(dir, "new.db")
	if err := os.WriteFile(src, []byte("session-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "session-bytes" {
		t.Errorf("copied content = %q, want session-bytes", got)
	}
}
