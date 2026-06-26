package filesystem

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCreate(t *testing.T, root string, in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := NewCreateFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, raw)
	return res.Content, err
}

func TestCreateFileNew(t *testing.T) {
	root := t.TempDir()
	content, err := runCreate(t, root, map[string]any{
		"path":    "hello.go",
		"content": "package main\n\nfunc main() {}\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "Created hello.go") {
		t.Errorf("expected creation message, got: %s", content)
	}
	got, err := os.ReadFile(filepath.Join(root, "hello.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "package main\n\nfunc main() {}\n" {
		t.Errorf("file content mismatch: %q", got)
	}
}

func TestCreateFileAlreadyExists(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "exists.txt"), []byte("old"), 0o644)
	content, err := runCreate(t, root, map[string]any{
		"path":    "exists.txt",
		"content": "new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "already exists") {
		t.Errorf("expected 'already exists' message, got: %s", content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "exists.txt"))
	if string(got) != "old" {
		t.Errorf("existing file should be unchanged, got: %q", got)
	}
}

func TestCreateFileIntermediateDirs(t *testing.T) {
	root := t.TempDir()
	_, err := runCreate(t, root, map[string]any{
		"path":    "a/b/c/deep.txt",
		"content": "nested",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a", "b", "c", "deep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nested" {
		t.Errorf("file content mismatch: %q", got)
	}
}

func TestCreateFilePathEscapeIsError(t *testing.T) {
	root := t.TempDir()
	_, err := runCreate(t, root, map[string]any{
		"path":    "../escape.txt",
		"content": "nope",
	})
	if err == nil {
		t.Fatal("expected an error for a path escaping the workspace")
	}
}
