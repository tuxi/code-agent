package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/tools"
)

func TestEditContractRejectsMissingOrMisspelledReplacement(t *testing.T) {
	for _, input := range []string{
		`{"path":"f.txt","old":"value","allow_delete":true}`,
		`{"path":"f.txt","old":"value","new_string":"replacement"}`,
		`{"path":"f.txt","old":"value","new":"replacement","new_string":"wrong"}`,
		`{"path":"f.txt","old":"value","new":null,"allow_delete":true}`,
	} {
		root := tempWith(t, "value\n")
		result, err := NewEditFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(input))
		if err == nil || len(result.Wrote) != 0 {
			t.Fatalf("unsafe input accepted: %s %+v %v", input, result, err)
		}
		got, _ := os.ReadFile(filepath.Join(root, "f.txt"))
		if string(got) != "value\n" {
			t.Fatalf("rejected input modified file: %q", got)
		}
	}
}

func TestReadRawTextCanBeEditedWithoutChangingOtherLineEndings(t *testing.T) {
	for _, original := range []string{"head\r\n\t中文 old\r\ntail\r\n", "head\r\n\t中文 old\ntail\r\n"} {
		root := tempWith(t, original)
		ec := tools.ExecutionContext{WorkspaceRoot: root}
		read, err := NewReadFileTool().Execute(context.Background(), ec, json.RawMessage(`{"path":"f.txt","offset":2,"limit":1,"line_numbers":false}`))
		if err != nil || read.Content != "\t中文 old" {
			t.Fatalf("raw read=%q %v", read.Content, err)
		}
		input, _ := json.Marshal(map[string]any{"path": "f.txt", "old": read.Content, "new": "\t中文 new"})
		result, err := NewEditFileTool().Execute(context.Background(), ec, input)
		if err != nil || result.Failure != nil {
			t.Fatalf("edit=%+v %v", result, err)
		}
		got, _ := os.ReadFile(filepath.Join(root, "f.txt"))
		if string(got) != strings.Replace(original, "old", "new", 1) {
			t.Fatalf("unrelated bytes changed: %q", got)
		}
	}
}

func TestEditCRLFSpanAndExplicitDeletion(t *testing.T) {
	for _, tc := range []struct {
		old, replacement, want string
		deleting               bool
	}{
		{"a\nb\n", "c\nd\n", "c\r\nd\r\ntail\r\n", false},
		{"\nb", "\nx", "a\r\nx\r\ntail\r\n", false},
		{"b\n", "", "a\r\ntail\r\n", true},
	} {
		root := tempWith(t, "a\r\nb\r\ntail\r\n")
		input, _ := json.Marshal(map[string]any{"path": "f.txt", "old": tc.old, "new": tc.replacement, "allow_delete": tc.deleting})
		result, err := NewEditFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, input)
		if err != nil || result.Failure != nil {
			t.Fatalf("edit=%+v %v", result, err)
		}
		got, _ := os.ReadFile(filepath.Join(root, "f.txt"))
		if string(got) != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func TestEditRefusalIsExplicitAndDiagnosticsRespectReadProtection(t *testing.T) {
	root := tempWith(t, "value\n")
	result, err := NewEditFileTool().Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(`{"path":"f.txt","old":"value","new":""}`))
	if err != nil || result.Failure == nil || result.Failure.Kind != "blocked" || result.Err() == nil {
		t.Fatalf("missing refusal: %+v %v", result, err)
	}
	if got := editDiagnostic(tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(`{"path":".env"}`), "SECRET=value", "SECRET=wrong", "replacement"); got != "" {
		t.Fatalf("protected content diagnostic: %s", got)
	}
	long := strings.Repeat("中", 1000)
	if got := nearMissDiagnostic("prefix"+long, "prefixwrong"); len([]rune(got)) > 1000 {
		t.Fatalf("unbounded diagnostic: %d", len(got))
	}
}
