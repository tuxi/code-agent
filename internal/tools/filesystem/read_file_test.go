package filesystem

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = "line one\nline two\nline three\nline four\nline five\n"

func tempWith(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func read(t *testing.T, root, raw string) (string, error) {
	t.Helper()
	res, err := NewReadFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(raw))
	return res.Content, err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestReadFullWithLineNumbers(t *testing.T) {
	out, err := read(t, tempWith(t, sample), `{"path":"f.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "1\tline one") {
		t.Errorf("expected a line-numbered first line, got %q", firstLine(out))
	}
	if !strings.Contains(out, "5\tline five") {
		t.Errorf("expected the last line numbered:\n%s", out)
	}
	if n := strings.Count(out, "\n") + 1; n != 5 {
		t.Errorf("expected 5 lines, got %d:\n%s", n, out)
	}
}

func TestReadOffset(t *testing.T) {
	out, _ := read(t, tempWith(t, sample), `{"path":"f.txt","offset":3}`)
	if !strings.HasPrefix(out, "3\tline three") {
		t.Errorf("offset 3 should start at line 3, got %q", firstLine(out))
	}
	if strings.Contains(out, "line two") {
		t.Error("offset 3 should skip line 2")
	}
}

func TestReadLimit(t *testing.T) {
	out, _ := read(t, tempWith(t, sample), `{"path":"f.txt","limit":2}`)
	if n := strings.Count(out, "\n") + 1; n != 2 {
		t.Errorf("limit 2 → %d lines:\n%s", n, out)
	}
	if !strings.HasPrefix(out, "1\tline one") {
		t.Errorf("got %q", firstLine(out))
	}
}

func TestReadOffsetAndLimit(t *testing.T) {
	out, _ := read(t, tempWith(t, sample), `{"path":"f.txt","offset":2,"limit":2}`)
	if !strings.HasPrefix(out, "2\tline two") {
		t.Errorf("got %q", firstLine(out))
	}
	if strings.Contains(out, "line four") {
		t.Error("limit 2 from offset 2 should stop at line 3")
	}
}

func TestReadOffsetBeyondEOFIsEmpty(t *testing.T) {
	out, err := read(t, tempWith(t, sample), `{"path":"f.txt","offset":999}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("offset beyond EOF should be empty, got %q", out)
	}
}

func TestReadStringAndMixedNumbers(t *testing.T) {
	root := tempWith(t, sample)

	out, err := read(t, root, `{"path":"f.txt","offset":"2","limit":"2"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "2\tline two") {
		t.Errorf("string numbers: got %q", firstLine(out))
	}

	out2, err := read(t, root, `{"path":"f.txt","offset":4,"limit":"1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out2, "\n") != 0 || !strings.HasPrefix(out2, "4\tline four") {
		t.Errorf("mixed types: got %q", out2)
	}
}

func TestReadInvalidNumber(t *testing.T) {
	if _, err := read(t, tempWith(t, sample), `{"path":"f.txt","offset":"abc"}`); err == nil {
		t.Error("expected an error for a non-numeric offset")
	}
}

func TestReadPathRequired(t *testing.T) {
	if _, err := read(t, tempWith(t, sample), `{}`); err == nil {
		t.Error("expected an error when path is missing")
	}
}

func TestReadLargeFileFullErrorsWindowWorks(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 50000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewReadFileTool()
	tool.MaxBytes = 10_000 // force the file to count as "too large to read whole"

	// Full read is rejected, with a hint to window.
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, json.RawMessage(`{"path":"big.txt"}`))
	if err == nil {
		t.Fatal("full read of an over-budget file should error")
	}
	if !strings.Contains(err.Error(), "offset/limit") {
		t.Errorf("error should hint at offset/limit, got: %v", err)
	}

	// A windowed read of the same large file works.
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: dir}, json.RawMessage(`{"path":"big.txt","offset":100,"limit":3}`))
	if err != nil {
		t.Fatalf("windowed read of a large file should work: %v", err)
	}
	if !strings.Contains(res.Content, "\tline 100") || !strings.Contains(res.Content, "\tline 102") {
		t.Errorf("windowed read returned the wrong slice:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "\tline 103") {
		t.Errorf("limit 3 should stop at line 102:\n%s", res.Content)
	}
}

func TestReadNilContextTolerated(t *testing.T) {
	root := tempWith(t, sample)
	if _, err := NewReadFileTool().Execute(nil, tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(`{"path":"f.txt"}`)); err != nil {
		t.Errorf("a nil context should be tolerated, got %v", err)
	}
}
