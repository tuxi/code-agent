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

func runGoGitApply(t *testing.T, dir, patch string) tools.ToolResult {
	t.Helper()
	tool := NewApplyPatchToolGoGit()
	input, _ := json.Marshal(applyPatchInput{Patch: patch})
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func TestGoGitApply_ModifyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"

	res := runGoGitApply(t, dir, patch)
	if !strings.Contains(res.Content, "applied successfully") {
		t.Fatalf("apply failed: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != "a\nB\nc\n" {
		t.Errorf("content = %q, want %q", got, "a\nB\nc\n")
	}
}

func TestGoGitApply_NewFile(t *testing.T) {
	dir := t.TempDir()
	patch := "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\n"

	res := runGoGitApply(t, dir, patch)
	if !strings.Contains(res.Content, "applied successfully") {
		t.Fatalf("apply failed: %s", res.Content)
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatalf("new file not created: %v", err)
	}
	if string(got) != "hello\nworld\n" {
		t.Errorf("content = %q", got)
	}
}

func TestGoGitApply_DeleteFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gone.txt"), []byte("bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n"

	res := runGoGitApply(t, dir, patch)
	if !strings.Contains(res.Content, "applied successfully") {
		t.Fatalf("apply failed: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("file should be deleted, stat err = %v", err)
	}
}

// A patch whose context does not match must change nothing and report the failure.
func TestGoGitApply_BadPatchChangesNothing(t *testing.T) {
	dir := t.TempDir()
	original := "a\nb\nc\n"
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// Context line "X" does not exist in the file → must not apply.
	patch := "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n X\n-b\n+B\n c\n"

	res := runGoGitApply(t, dir, patch)
	if strings.Contains(res.Content, "applied successfully") {
		t.Fatalf("a non-matching patch should not apply: %s", res.Content)
	}
	if !strings.Contains(res.Content, "no files were changed") {
		t.Errorf("missing no-change message: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != original {
		t.Errorf("file was modified despite failed patch: %q", got)
	}
}

// A patch targeting a path outside the workspace must be refused.
func TestGoGitApply_RejectsEscape(t *testing.T) {
	dir := t.TempDir()
	patch := "--- /dev/null\n+++ b/../../escape.txt\n@@ -0,0 +1 @@\n+nope\n"

	res := runGoGitApply(t, dir, patch)
	if strings.Contains(res.Content, "applied successfully") {
		t.Fatalf("escape patch should be refused: %s", res.Content)
	}
	if !strings.Contains(res.Content, "escapes workspace") {
		t.Errorf("missing escape rejection: %s", res.Content)
	}
}
