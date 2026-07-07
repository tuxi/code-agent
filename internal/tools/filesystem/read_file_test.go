package filesystem

import (
	"code-agent/internal/sandbox"
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

func TestReadFileEmitsFileAsset(t *testing.T) {
	root := tempWith(t, sample)
	res, err := NewReadFileTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_1",
		CallID:        "call_read",
	}, json.RawMessage(`{"path":"f.txt","offset":2,"limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(res.Assets))
	}
	ref := res.Assets[0]
	if ref.Kind != "file" || ref.WorkspaceRelativePath != "f.txt" {
		t.Fatalf("asset = %+v", ref)
	}
	if ref.Range == nil || ref.Range.StartLine != 2 || ref.Range.EndLine != 3 {
		t.Fatalf("range = %+v, want 2..3", ref.Range)
	}
	var out readFileOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output is not readFileOutput: %v\n%s", err, res.Output)
	}
	if out.Kind != "file" || out.AssetID != ref.ID || out.DisplayRange.StartLine != 2 || out.LineCount != 2 {
		t.Fatalf("output = %+v, asset id = %q", out, ref.ID)
	}
}

func TestReadFileInspect(t *testing.T) {
	tool := NewReadFileTool()

	t.Run("normal path passes inspect", func(t *testing.T) {
		err := tool.Inspect(json.RawMessage(`{"path": "main.go"}`), "/tmp/ws")
		if err != nil {
			t.Errorf("normal path should pass inspect, got: %v", err)
		}
	})

	t.Run("empty path passes inspect", func(t *testing.T) {
		err := tool.Inspect(json.RawMessage(`{"path": ""}`), "/tmp/ws")
		if err != nil {
			t.Errorf("empty path should pass inspect (Execute surfaces error), got: %v", err)
		}
	})

	t.Run("malformed input passes inspect", func(t *testing.T) {
		err := tool.Inspect(json.RawMessage(`not json`), "/tmp/ws")
		if err != nil {
			t.Errorf("malformed input should pass inspect (Execute surfaces error), got: %v", err)
		}
	})

	t.Run("empty input passes inspect", func(t *testing.T) {
		err := tool.Inspect(nil, "/tmp/ws")
		if err != nil {
			t.Errorf("nil input should pass inspect, got: %v", err)
		}
	})

	// Protected paths
	for _, path := range []string{
		".env", ".env.local", ".env.production", ".env.staging",
		".git-credentials",
		"credentials", "secrets", "tokens",
		"private.key", "id_rsa", "id_ed25519",
	} {
		t.Run("protected path blocked: "+path, func(t *testing.T) {
			err := tool.Inspect(json.RawMessage(`{"path": "`+path+`"}`), "/tmp/ws")
			if err == nil {
				t.Errorf("protected path %q should be blocked by inspect", path)
			}
		})
	}

	t.Run("deep protected path blocked", func(t *testing.T) {
		err := tool.Inspect(json.RawMessage(`{"path": "config/.env.production"}`), "/tmp/ws")
		if err == nil {
			t.Error("deep protected path should be blocked by inspect")
		}
	})

	t.Run("non-protected path passes", func(t *testing.T) {
		for _, path := range []string{"main.go", "README.md", "cmd/server/main.go", "pkg/util/config.go"} {
			err := tool.Inspect(json.RawMessage(`{"path": "`+path+`"}`), "/tmp/ws")
			if err != nil {
				t.Errorf("non-protected path %q should pass inspect, got: %v", path, err)
			}
		}
	})
}

func TestIsPathProtected(t *testing.T) {
	pp := sandbox.ProtectedPaths(nil)

	t.Run("exact base name", func(t *testing.T) {
		if !sandbox.IsPathProtected(".env", pp) {
			t.Error(".env should be protected")
		}
	})

	t.Run("deep path with protected base", func(t *testing.T) {
		if !sandbox.IsPathProtected("apps/backend/.env.production", pp) {
			t.Error("apps/backend/.env.production should be protected")
		}
	})

	t.Run("normal file is not protected", func(t *testing.T) {
		if sandbox.IsPathProtected("main.go", pp) {
			t.Error("main.go should not be protected")
		}
	})

	t.Run("case insensitive match", func(t *testing.T) {
		if !sandbox.IsPathProtected(".ENV", pp) {
			t.Error(".ENV should be protected (case insensitive)")
		}
	})

	t.Run("glob pattern *.key", func(t *testing.T) {
		if !sandbox.IsPathProtected("server.key", pp) {
			t.Error("server.key should be protected by *.key glob")
		}
	})

	t.Run("glob pattern *.pem", func(t *testing.T) {
		if !sandbox.IsPathProtected("ca.pem", pp) {
			t.Error("ca.pem should be protected by *.pem glob")
		}
	})
}
