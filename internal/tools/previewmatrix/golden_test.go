package previewmatrix

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/preview"
)

var updateGolden = flag.Bool("update", false, "update golden ui_preview_matrix result fixtures")

// goldenResult exercises the three backend cell outcomes in one tool call: two
// completed adaptation previews, one failed preview revision that keeps its
// recorded identity, and one missing cell that claims nothing.
func goldenResult(t *testing.T) result {
	t.Helper()
	fx := newFixture(t, nil, nil)
	matrixID := "matrix_p1g_tool_golden"
	fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:ipad-default", Lineage: fx.lineage("ipad_landscape", "default", matrixID),
	})
	fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:ipad-loading", Lineage: fx.lineage("ipad_landscape", "loading", matrixID),
		Status: preview.StatusFailed, Code: preview.CodeViewerFetchFailed, Message: "viewer bytes could not be fetched",
	})
	fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:iphone-default", Lineage: fx.lineage("iphone_portrait", "default", matrixID),
	})
	input := fx.baseInput(matrixID)
	input["entries"] = []any{
		map[string]any{"target": "ipad_landscape", "state": "default", "preview_ref": keyRef("ui-adaptation:ipad-default")},
		map[string]any{"target": "ipad_landscape", "state": "loading", "preview_ref": keyRef("ui-adaptation:ipad-loading")},
		map[string]any{"target": "iphone_portrait", "state": "default", "preview_ref": keyRef("ui-adaptation:iphone-default")},
	}
	return decodeResult(t, execute(t, fx, input))
}

func TestGoldenResult(t *testing.T) {
	got, err := json.MarshalIndent(goldenResult(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "golden", "result.json")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run go test ./internal/tools/previewmatrix -update)", path, err)
	}
	if string(want) != string(got) {
		t.Fatalf("golden %s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
