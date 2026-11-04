package git

import (
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/tools"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func onlyPatchWrite(t *testing.T, res tools.ToolResult) tools.FileWrite {
	t.Helper()
	if len(res.Wrote) != 1 {
		t.Fatalf("Wrote = %+v, want exactly one entry", res.Wrote)
	}
	return res.Wrote[0]
}

func TestApplyPatchReportsModifiedLines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f.txt", "a\nb\nc\n")
	// One line replaced, one line added.
	patch := "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,4 @@\n a\n-b\n+B\n+extra\n c\n"

	w := onlyPatchWrite(t, runGoGitApply(t, dir, patch))
	if w.Path != "f.txt" || w.Status != tools.WriteModified {
		t.Fatalf("write = %+v, want f.txt modified", w)
	}
	if w.Added != 2 || w.Removed != 1 {
		t.Errorf("added/removed = %d/%d, want 2/1", w.Added, w.Removed)
	}
}

func TestApplyPatchReportsNewFile(t *testing.T) {
	dir := t.TempDir()
	patch := "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\n"

	w := onlyPatchWrite(t, runGoGitApply(t, dir, patch))
	if w.Path != "new.txt" || w.Status != tools.WriteAdded {
		t.Fatalf("write = %+v, want new.txt added", w)
	}
	if w.Added != 2 || w.Removed != 0 {
		t.Errorf("added/removed = %d/%d, want 2/0", w.Added, w.Removed)
	}
}

// A whole-file delete is the only path that produces status=deleted in v1.
func TestApplyPatchReportsDeletedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "gone.txt", "a\nb\n")
	patch := "--- a/gone.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-a\n-b\n"

	w := onlyPatchWrite(t, runGoGitApply(t, dir, patch))
	if w.Path != "gone.txt" || w.Status != tools.WriteDeleted {
		t.Fatalf("write = %+v, want gone.txt deleted", w)
	}
	if w.Removed != 2 || w.Added != 0 {
		t.Errorf("added/removed = %d/%d, want 0/2", w.Added, w.Removed)
	}
}

func TestApplyPatchReportsEveryFileInMultiFilePatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "one.txt", "a\n")
	patch := "--- a/one.txt\n+++ b/one.txt\n@@ -1 +1,2 @@\n a\n+b\n" +
		"--- /dev/null\n+++ b/two.txt\n@@ -0,0 +1 @@\n+only\n"

	res := runGoGitApply(t, dir, patch)
	if len(res.Wrote) != 2 {
		t.Fatalf("Wrote = %+v, want two entries", res.Wrote)
	}
	byPath := map[string]tools.FileWrite{}
	for _, w := range res.Wrote {
		byPath[w.Path] = w
	}
	if byPath["one.txt"].Added != 1 || byPath["one.txt"].Status != tools.WriteModified {
		t.Errorf("one.txt = %+v", byPath["one.txt"])
	}
	if byPath["two.txt"].Added != 1 || byPath["two.txt"].Status != tools.WriteAdded {
		t.Errorf("two.txt = %+v", byPath["two.txt"])
	}
}

// A patch that does not apply changes nothing, so it must report nothing: the
// counters cannot depend on the observation text.
func TestApplyPatchFailureReportsNoWrites(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f.txt", "a\n")
	patch := "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-zzz\n+y\n"

	res := runGoGitApply(t, dir, patch)
	if len(res.Wrote) != 0 {
		t.Fatalf("failed patch reported writes: %+v", res.Wrote)
	}
}
