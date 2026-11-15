package matrix

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/preview"
)

var updateGolden = flag.Bool("update", false, "update golden matrix fixtures")

// goldenMatrix exercises all three cell statuses in one matrix: a completed
// adaptation preview, a failed preview revision that must not donate its
// fallback, another completed cell, and a cell with no preview at all.
func goldenMatrix(t *testing.T) Matrix {
	t.Helper()
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_golden"
	importArtifact(t, store, artifactOptions{
		ID: "preview_golden_ipad_default", Lineage: testLineage(ws, "ipad_landscape", "default", matrixID),
	})
	importArtifact(t, store, artifactOptions{
		ID: "preview_golden_ipad_loading", Lineage: testLineage(ws, "ipad_landscape", "loading", matrixID),
		Status: preview.StatusFailed, ErrorCode: preview.CodeViewerFetchFailed, Message: "viewer bytes could not be fetched",
	})
	importArtifact(t, store, artifactOptions{
		ID: "preview_golden_iphone_default", Lineage: testLineage(ws, "iphone_portrait", "default", matrixID),
	})
	cells := []CellRef{
		{Target: "ipad_landscape", State: "default", PreviewArtifactID: "preview_golden_ipad_default", PreviewRevision: 1},
		{Target: "ipad_landscape", State: "loading", PreviewArtifactID: "preview_golden_ipad_loading", PreviewRevision: 1},
		{Target: "iphone_portrait", State: "default", PreviewArtifactID: "preview_golden_iphone_default", PreviewRevision: 1},
	}
	return mustBuild(t, builder, buildRequest(ws, matrixID, cells))
}

func TestGoldenMatrixAndManifest(t *testing.T) {
	document := goldenMatrix(t)
	matrixBytes, err := Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := BuildManifest(document).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	compareGolden(t, filepath.Join("testdata", "golden", "matrix.json"), matrixBytes)
	compareGolden(t, filepath.Join("testdata", "golden", "manifest.json"), manifestBytes)

	if document.Coverage != (Coverage{Targets: 2, States: 2, Expected: 4, Completed: 2, Failed: 1, Missing: 1}) {
		t.Fatalf("golden coverage = %+v", document.Coverage)
	}
}

func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
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
		t.Fatalf("read golden %s: %v (run go test ./internal/preview/matrix -update)", path, err)
	}
	if string(want) != string(got) {
		t.Fatalf("golden %s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
