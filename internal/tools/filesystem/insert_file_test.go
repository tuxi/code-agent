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

func runInsert(t *testing.T, root string, in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := NewInsertFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, raw)
	return res.Content, err
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInsertFileInsertsAfterLine(t *testing.T) {
	root, rel := writeTemp(t, "f.md", "intro\n1. 配置库共 12 个文件\n3. **没有 MDM.plist\noutro\n")
	content, err := runInsert(t, root, map[string]any{
		"path":    rel,
		"line":    2,
		"content": "2. 它不是 MDM 描述文件",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "intro\n1. 配置库共 12 个文件\n2. 它不是 MDM 描述文件\n3. **没有 MDM.plist\noutro\n"
	if got := readFile(t, root, rel); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if !strings.HasPrefix(content, "Inserted 1 line(s) after line 2 of") {
		t.Errorf("header should say what happened:\n%s", content)
	}
	if !strings.Contains(content, "+3\t2. 它不是 MDM 描述文件") {
		t.Errorf("diff should show the inserted line at line 3:\n%s", content)
	}
	if strings.Contains(content, "-3\t") {
		t.Errorf("a pure insert removes nothing:\n%s", content)
	}
}

// The whole point of the tool: content with quotes, backticks and CJK is
// inserted byte-for-byte, with no 'old' text to match.
func TestInsertFileContentIsVerbatim(t *testing.T) {
	root, rel := writeTemp(t, "f.md", "a\nb\n")
	block := "该 stub 的 `MCProfileIsRemovalStub=true`，内容是 \"Vodafone UK\" 的 Wi-Fi 描述文件"
	if _, err := runInsert(t, root, map[string]any{"path": rel, "line": 1, "content": block}); err != nil {
		t.Fatal(err)
	}
	want := "a\n" + block + "\nb\n"
	if got := readFile(t, root, rel); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestInsertFileNameAndSchemaMatchFamily(t *testing.T) {
	tool := NewInsertFileTool()
	if tool.Name() != "insert_file" {
		t.Errorf("name = %q, want insert_file", tool.Name())
	}
	// Sibling naming: create_file / edit_file / insert_file.
	for _, other := range []string{"create_file", "edit_file"} {
		if !strings.HasSuffix(other, "_file") || !strings.HasSuffix(tool.Name(), "_file") {
			t.Errorf("%s and %s should share the <verb>_file shape", other, tool.Name())
		}
	}
	if !strings.Contains(string(tool.InputSchema()), `"line"`) {
		t.Errorf("schema must require a line address: %s", tool.InputSchema())
	}
}

func TestInsertFileLineZeroInsertsAtTop(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\nb\n")
	if _, err := runInsert(t, root, map[string]any{"path": rel, "line": 0, "content": "header"}); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, root, rel), "header\na\nb\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestInsertFilePreservesMissingTrailingNewline(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\nb")
	if _, err := runInsert(t, root, map[string]any{"path": rel, "line": 1, "content": "middle"}); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, root, rel), "a\nmiddle\nb"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestInsertFileAppendsAfterLastLine(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\nb\n")
	if _, err := runInsert(t, root, map[string]any{"path": rel, "line": 2, "content": "c\n\nd"}); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, root, rel), "a\nb\nc\n\nd\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestInsertFileEmptyFile(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "")
	if _, err := runInsert(t, root, map[string]any{"path": rel, "line": 0, "content": "first"}); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, root, rel), "first\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// An out-of-range line is the model's mistake to fix, so it must come back as a
// recoverable observation that states the range — and must not touch the file.
func TestInsertFileLineOutOfRangeIsRecoverable(t *testing.T) {
	const original = "a\nb\n"
	root, rel := writeTemp(t, "f.txt", original)
	content, err := runInsert(t, root, map[string]any{"path": rel, "line": 9, "content": "x"})
	if err != nil {
		t.Fatalf("expected a recoverable observation, got error: %v", err)
	}
	if !strings.Contains(content, "has 2 line(s)") {
		t.Errorf("observation should state the real line count: %s", content)
	}
	if got := readFile(t, root, rel); got != original {
		t.Errorf("file must be untouched, got %q", got)
	}
}

func TestInsertFileRequiresContent(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\n")
	if _, err := runInsert(t, root, map[string]any{"path": rel, "line": 1, "content": ""}); err == nil {
		t.Error("empty content should be rejected")
	}
	if got := readFile(t, root, rel); got != "a\n" {
		t.Errorf("file must be untouched, got %q", got)
	}
}

func TestInsertFileMissingFileIsRecoverable(t *testing.T) {
	root := t.TempDir()
	content, err := runInsert(t, root, map[string]any{"path": "nope.txt", "line": 0, "content": "x"})
	if err != nil {
		t.Fatalf("expected a recoverable observation, got error: %v", err)
	}
	if !strings.Contains(content, "Could not open") {
		t.Errorf("expected an open failure, got: %s", content)
	}
}

// CRLF input is inserted as LF, matching edit_file and read_file's LF view.
func TestInsertFileNormalizesCRLF(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\r\nb\r\n")
	if _, err := runInsert(t, root, map[string]any{"path": rel, "line": 1, "content": "x\r\ny"}); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, root, rel), "a\nx\ny\nb\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestInsertFilePlanModeRefusesProjectFiles(t *testing.T) {
	root, rel := writeTemp(t, "f.txt", "a\n")
	raw, _ := json.Marshal(map[string]any{"path": rel, "line": 1, "content": "x"})
	_, err := NewInsertFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root, PlanMode: true}, raw)
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Errorf("expected a plan-mode refusal, got %v", err)
	}
	if got := readFile(t, root, rel); got != "a\n" {
		t.Errorf("file must be untouched, got %q", got)
	}
}
