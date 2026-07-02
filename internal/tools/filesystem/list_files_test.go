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

func TestListFilesEmitsDirectoryListingAssets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AI热点速递_2026年6月28-30日.md"), []byte("# AI"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "README.md"), []byte("# Skills"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "style-guide.md"), []byte("# Style"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := NewListFilesTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_1",
		CallID:        "call_list",
	}, json.RawMessage(`{"path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"AI热点速递_2026年6月28-30日.md",
		"skills/",
		"skills/README.md",
		"skills/style-guide.md",
	} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("content missing %q:\n%s", want, res.Content)
		}
	}
	if len(res.Assets) != 4 {
		t.Fatalf("assets = %d, want 4: %+v", len(res.Assets), res.Assets)
	}

	byPath := map[string]string{}
	for _, ref := range res.Assets {
		byPath[ref.WorkspaceRelativePath] = ref.Kind
		if ref.SourceTurnID != "turn_1" || ref.SourceCallID != "call_list" {
			t.Fatalf("missing source ids on asset: %+v", ref)
		}
	}
	if byPath["AI热点速递_2026年6月28-30日.md"] != "file" {
		t.Fatalf("markdown asset kind = %q", byPath["AI热点速递_2026年6月28-30日.md"])
	}
	if byPath["skills"] != "directory" {
		t.Fatalf("skills asset kind = %q", byPath["skills"])
	}
	if byPath["skills/README.md"] != "file" || byPath["skills/style-guide.md"] != "file" {
		t.Fatalf("nested file assets = %+v", byPath)
	}

	var out listFilesOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output is not listFilesOutput: %v\n%s", err, res.Output)
	}
	if out.Kind != "directory_listing" || out.Path != "." || len(out.Items) != 4 {
		t.Fatalf("output = %+v", out)
	}
	for _, item := range out.Items {
		if item.AssetID == "" || item.Path == "" || item.Kind == "" {
			t.Fatalf("incomplete output item: %+v", item)
		}
	}
}
