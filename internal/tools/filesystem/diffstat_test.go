package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/tools"
)

// execWrite runs one write tool the way the runner does and returns its result.
func execWrite(t *testing.T, tool tools.Tool, root string, in map[string]any) tools.ToolResult {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, raw)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func onlyWrite(t *testing.T, res tools.ToolResult) tools.FileWrite {
	t.Helper()
	if len(res.Wrote) != 1 {
		t.Fatalf("Wrote = %+v, want exactly one entry", res.Wrote)
	}
	return res.Wrote[0]
}

func TestCreateFileReportsWrote(t *testing.T) {
	root := t.TempDir()
	// Nested path: create_file makes the parent directory, and the report must be
	// the workspace-relative path the client sees, not the model's argument.
	res := execWrite(t, NewCreateFileTool(), root, map[string]any{"path": "a/b.md", "content": "x\ny\n"})
	w := onlyWrite(t, res)
	if w.Path != "a/b.md" {
		t.Errorf("path = %q, want a/b.md", w.Path)
	}
	if w.Added != 2 || w.Removed != 0 {
		t.Errorf("added/removed = %d/%d, want 2/0", w.Added, w.Removed)
	}
	if w.Status != tools.WriteAdded {
		t.Errorf("status = %q, want %q", w.Status, tools.WriteAdded)
	}
}

func TestCreateFileRejectsExistingWithoutWrote(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := execWrite(t, NewCreateFileTool(), root, map[string]any{"path": "f.txt", "content": "new\n"})
	if len(res.Wrote) != 0 {
		t.Fatalf("a refused create reported writes: %+v", res.Wrote)
	}
}

func TestEditFileReportsWrote(t *testing.T) {
	root, rel := writeTemp(t, "f.go", "package x\n\nfunc A() {}\n")
	res := execWrite(t, NewEditFileTool(), root, map[string]any{
		"path": rel, "old": "func A()", "new": "func B()\nfunc C()",
	})
	w := onlyWrite(t, res)
	if w.Path != rel || w.Status != tools.WriteModified {
		t.Fatalf("write = %+v, want %s modified", w, rel)
	}
	if w.Added != 2 || w.Removed != 1 {
		t.Errorf("added/removed = %d/%d, want 2/1", w.Added, w.Removed)
	}
}

// allow_delete deletes text inside the file, not the file: the status must stay
// modified or the UI would announce a deleted file that still exists.
func TestEditFileDeleteTextStaysModified(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "keep\ndrop\nkeep2\n")
	res := execWrite(t, NewEditFileTool(), root, map[string]any{
		"path": rel, "old": "drop", "new": "", "allow_delete": true,
	})
	w := onlyWrite(t, res)
	if w.Status != tools.WriteModified {
		t.Fatalf("status = %q, want modified", w.Status)
	}
	if w.Added != 0 || w.Removed != 1 {
		t.Errorf("added/removed = %d/%d, want 0/1", w.Added, w.Removed)
	}
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		t.Fatalf("file disappeared: %v", err)
	}
}

// The recoverable failures (§4.2.1) return an observation with a nil error, so
// only the absence of Wrote keeps them out of the turn's counts.
func TestEditFileRetryDoesNotReportWrote(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "alpha\nbeta\n")
	for _, in := range []map[string]any{
		{"path": rel, "old": "gamma", "new": "x"},
		{"path": rel, "old": "", "new": "x"},
		{"path": rel, "old": "alpha", "new": "alpha"},
		{"path": rel, "old": "a", "new": "b"},
	} {
		res := execWrite(t, NewEditFileTool(), root, in)
		if len(res.Wrote) != 0 {
			t.Errorf("input %+v reported writes: %+v", in, res.Wrote)
		}
	}
}

func TestInsertFileReportsWrote(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\nc\n")
	res := execWrite(t, NewInsertFileTool(), root, map[string]any{
		"path": rel, "line": 1, "content": "b1\nb2",
	})
	w := onlyWrite(t, res)
	if w.Added != 2 || w.Removed != 0 || w.Status != tools.WriteModified {
		t.Fatalf("write = %+v, want 2 added, 0 removed, modified", w)
	}
}

func TestInsertFileOutOfRangeDoesNotReportWrote(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\n")
	res := execWrite(t, NewInsertFileTool(), root, map[string]any{
		"path": rel, "line": 99, "content": "x\n",
	})
	if len(res.Wrote) != 0 {
		t.Fatalf("out-of-range insert reported writes: %+v", res.Wrote)
	}
}
