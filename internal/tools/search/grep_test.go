package search

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/tools"
)

func TestGrepEmitsSearchAssets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ConversationState.swift"), []byte("public var streamingText: String = \"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := NewGrepTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_2",
		CallID:        "call_grep",
	}, json.RawMessage(`{"query":"streamingText","path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "ConversationState.swift:1: public var streamingText: String = \"\"" {
		t.Fatalf("content changed:\n%s", res.Content)
	}
	if len(res.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(res.Assets))
	}
	ref := res.Assets[0]
	if ref.Kind != "file_location" || ref.WorkspaceRelativePath != "ConversationState.swift" {
		t.Fatalf("asset = %+v", ref)
	}
	if ref.Range == nil || ref.Range.StartLine != 1 || ref.Range.StartColumn != 12 {
		t.Fatalf("asset range = %+v, want line 1 column 12", ref.Range)
	}
	if ref.MIMEType != "text/x-swift" {
		t.Fatalf("mime = %q", ref.MIMEType)
	}

	var out grepOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output is not grepOutput: %v\n%s", err, res.Output)
	}
	if out.Kind != "search_results" || len(out.Items) != 1 || out.Items[0].AssetID != ref.ID {
		t.Fatalf("output = %+v, asset id = %q", out, ref.ID)
	}
}

func TestGrepRegexQuery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cfg.go"), []byte("cfg.Hooks = append(cfg.Hooks, sf.Hooks...)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := NewGrepTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_2",
		CallID:        "call_grep",
	}, json.RawMessage(`{"query":"\\.Hooks","path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "cfg.go:1: cfg.Hooks = append(cfg.Hooks, sf.Hooks...)" {
		t.Fatalf("regex query content:\n%s", res.Content)
	}
}

func TestGrepRegexAnchors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("var x = 1\nx = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := NewGrepTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_3",
		CallID:        "call_grep",
	}, json.RawMessage(`{"query":"^x = ","path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "a.go:2: x = 2" {
		t.Fatalf("anchored regex content:\n%s", res.Content)
	}
}

func TestGrepInvalidRegexFallsBackToLiteral(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cpp.go"), []byte("// C++ style\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// "C++" is not a valid RE2 pattern; must fall back to a literal match.
	res, err := NewGrepTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_4",
		CallID:        "call_grep",
	}, json.RawMessage(`{"query":"C++","path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "cpp.go:1: // C++ style" {
		t.Fatalf("literal fallback content:\n%s", res.Content)
	}
}
