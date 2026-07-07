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

func writeTemp(t *testing.T, name, content string) (root, rel string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, name
}

func runEdit(t *testing.T, root string, in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := NewEditFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, raw)
	return res.Content, err
}

func TestEditFileReplacesUnique(t *testing.T) {
	root, rel := writeTemp(t, "f.go", "package x\n\nfunc A() {}\n")
	if _, err := runEdit(t, root, map[string]any{"path": rel, "old": "func A()", "new": "func B()"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if !strings.Contains(string(got), "func B()") || strings.Contains(string(got), "func A()") {
		t.Fatalf("replacement not applied: %s", got)
	}
}

func TestEditFileNotFoundIsRecoverable(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "hello world\n")
	content, err := runEdit(t, root, map[string]any{"path": rel, "old": "goodbye", "new": "hi"})
	if err != nil {
		t.Fatalf("expected a recoverable observation, got error: %v", err)
	}
	if !strings.Contains(content, "Could not find") {
		t.Errorf("expected a not-found message, got: %s", content)
	}
	if got, _ := os.ReadFile(filepath.Join(root, rel)); string(got) != "hello world\n" {
		t.Errorf("file should be unchanged, got: %s", got)
	}
}

func TestEditFileAmbiguousIsRecoverable(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "x\nx\n")
	content, err := runEdit(t, root, map[string]any{"path": rel, "old": "x", "new": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "appears 2 times") {
		t.Errorf("expected an ambiguity message, got: %s", content)
	}
}

func TestEditFileDeletes(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "keep\nremove me\nkeep\n")
	if _, err := runEdit(t, root, map[string]any{"path": rel, "old": "remove me\n", "new": ""}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, rel)); strings.Contains(string(got), "remove me") {
		t.Errorf("text not deleted: %s", got)
	}
}

func TestEditDiffFormat(t *testing.T) {
	oldContent := "package p\n\n// old comment\noldCode()\n\n// after\n"
	newContent := "package p\n\n// new comment\nnewCode()\n\n// after\n"
	got := editDiff(oldContent, newContent, "// old comment\noldCode()", "// new comment\nnewCode()", 13, 1)

	if !strings.Contains(got, "-3\t// old comment") {
		t.Errorf("should show removed line with -:\n%s", got)
	}
	if !strings.Contains(got, "+3\t// new comment") {
		t.Errorf("should show added line with +:\n%s", got)
	}
	if !strings.Contains(got, " 2\t") {
		t.Errorf("should show context before with plain prefix:\n%s", got)
	}
	if !strings.Contains(got, "// after") {
		t.Errorf("should show context after:\n%s", got)
	}
}

func TestEditFileFoldsSmartQuotes(t *testing.T) {
	// File uses curly quotes; the model reproduces the line with straight ASCII
	// quotes (the transcript failure). The tolerant tier should still match.
	root, rel := writeTemp(t, "f.go", "// 表示一个“工作流产品”，定义模板\npackage x\n")
	content, err := runEdit(t, root, map[string]any{
		"path": rel,
		"old":  `// 表示一个"工作流产品"，定义模板`,
		"new":  "// 工作流定义",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "verify the result") {
		t.Errorf("expected a tolerant-match note, got: %s", content)
	}
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if string(got) != "// 工作流定义\npackage x\n" {
		t.Errorf("replacement not applied at the real byte span, got: %q", got)
	}
}

func TestEditFileStripsLineNumberPrefixes(t *testing.T) {
	// The model copies 'old' straight out of read_file, prefixes and all.
	root, rel := writeTemp(t, "f.go", "package x\n\nfunc A() {}\n")
	if _, err := runEdit(t, root, map[string]any{
		"path": rel,
		"old":  "3\tfunc A() {}",
		"new":  "func B() {}",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, rel))
	if !strings.Contains(string(got), "func B()") || strings.Contains(string(got), "func A()") {
		t.Errorf("prefixed replacement not applied: %s", got)
	}
}

func TestEditFileNearMissNamesTheDifferingRune(t *testing.T) {
	// A mismatch the tolerant tiers cannot fix: a mistyped identifier. The
	// not-found message should point at the closest line and the differing rune.
	root, rel := writeTemp(t, "f.go", "package x\n\nresult := compute(x)\n")
	content, err := runEdit(t, root, map[string]any{
		"path": rel,
		"old":  "result := kompute(x)",
		"new":  "result := run(x)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "closest line in the file is line 3") {
		t.Errorf("expected a near-miss pointer to line 3, got: %s", content)
	}
	if !strings.Contains(content, "First difference at column") {
		t.Errorf("expected a first-difference column, got: %s", content)
	}
	if got, _ := os.ReadFile(filepath.Join(root, rel)); string(got) != "package x\n\nresult := compute(x)\n" {
		t.Errorf("file should be unchanged after a near miss, got: %q", got)
	}
}

func TestEditFilePathEscapeIsError(t *testing.T) {
	root, _ := writeTemp(t, "f.txt", "hi\n")
	if _, err := runEdit(t, root, map[string]any{"path": "../escape.txt", "old": "hi", "new": "bye"}); err == nil {
		t.Fatal("expected an error for a path escaping the workspace")
	}
}
