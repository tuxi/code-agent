package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/preview"
)

func mustBuild(t *testing.T, builder *Builder, request Request) Matrix {
	t.Helper()
	document, err := builder.Build(context.Background(), request)
	if err != nil {
		t.Fatalf("build matrix: %v", err)
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("built matrix is invalid: %v", err)
	}
	return document
}

func mustFailBuild(t *testing.T, builder *Builder, request Request, wantCode string) error {
	t.Helper()
	_, err := builder.Build(context.Background(), request)
	if err == nil {
		t.Fatalf("build succeeded, want %s", wantCode)
	}
	if code := ErrorCode(err); code != wantCode {
		t.Fatalf("build error code = %s (%v), want %s", code, err, wantCode)
	}
	return err
}

// fullMatrix imports one completed preview per cell and returns the cells.
func fullMatrix(t *testing.T, ws testWorkspace, store *preview.FSStore, matrixID string) []CellRef {
	t.Helper()
	cells := make([]CellRef, 0, len(ws.Targets)*len(ws.States))
	index := 0
	for _, target := range ws.Targets {
		for _, state := range ws.States {
			id := fmt.Sprintf("preview_%s_%d", matrixID, index)
			index++
			importArtifact(t, store, artifactOptions{ID: id, Lineage: testLineage(ws, target, state, matrixID)})
			cells = append(cells, CellRef{Target: target, State: state, PreviewArtifactID: id, PreviewRevision: 1})
		}
	}
	return cells
}

func TestBuildFullMatrix(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_full"
	cells := fullMatrix(t, ws, store, matrixID)

	document := mustBuild(t, builder, buildRequest(ws, matrixID, cells))

	if document.Schema != Schema || document.MatrixID != matrixID {
		t.Fatalf("matrix identity = %s/%s", document.Schema, document.MatrixID)
	}
	if document.TargetMatrix.ID != matrixID || !sameStringSet(document.TargetMatrix.Targets, ws.Targets) {
		t.Fatalf("target matrix = %+v", document.TargetMatrix)
	}
	if !sameStringSet(document.States, ws.States) {
		t.Fatalf("states = %+v", document.States)
	}
	if document.Layout != ws.Identity {
		t.Fatalf("layout identity = %+v, want %+v", document.Layout, ws.Identity)
	}
	if document.SourceAsset != ws.Source || document.EvidenceSHA256 != nil {
		t.Fatalf("source identity = %+v evidence = %v", document.SourceAsset, document.EvidenceSHA256)
	}
	if !strings.HasPrefix(document.MatrixSHA256, "sha256:") {
		t.Fatalf("matrix digest = %q", document.MatrixSHA256)
	}
	if len(document.Entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(document.Entries))
	}
	if document.Coverage != (Coverage{Targets: 2, States: 2, Expected: 4, Completed: 4, Complete: true}) {
		t.Fatalf("coverage = %+v", document.Coverage)
	}
	fallbackSHA := sha256Hex(ws.FallbackBytes)
	for index, entry := range document.Entries {
		wantTarget := ws.Targets[index/len(ws.States)]
		wantState := ws.States[index%len(ws.States)]
		if entry.Target != wantTarget || entry.State != wantState {
			t.Fatalf("entry %d = %s/%s, want %s/%s", index, entry.Target, entry.State, wantTarget, wantState)
		}
		if entry.Status != StatusCompleted {
			t.Fatalf("entry %s/%s status = %s", entry.Target, entry.State, entry.Status)
		}
		if entry.Lineage == nil || entry.Lineage.Target != entry.Target || entry.Lineage.State != entry.State {
			t.Fatalf("entry %s/%s lineage = %+v", entry.Target, entry.State, entry.Lineage)
		}
		if entry.PreviewMode == nil || *entry.PreviewMode != string(preview.ModeAdaptation) {
			t.Fatalf("entry %s/%s mode = %v", entry.Target, entry.State, entry.PreviewMode)
		}
		if entry.FallbackAsset == nil || entry.FallbackAsset.SHA256 != fallbackSHA || entry.FallbackAsset.Width != 40 || entry.FallbackAsset.Height != 30 {
			t.Fatalf("entry %s/%s fallback = %+v", entry.Target, entry.State, entry.FallbackAsset)
		}
		if entry.Error != nil {
			t.Fatalf("entry %s/%s error = %+v", entry.Target, entry.State, entry.Error)
		}
	}
}

func TestBuildPartialSelection(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_partial"
	// A subset matrix is a different target set, so its previews must declare
	// that same set in their lineage.
	subset := []string{"iphone_portrait"}
	lineage := testLineage(ws, subset[0], ws.States[0], matrixID)
	lineage.TargetMatrix.Targets = subset
	importArtifact(t, store, artifactOptions{ID: "preview_p1g_partial_default", Lineage: lineage})
	loadingLineage := testLineage(ws, subset[0], ws.States[1], matrixID)
	loadingLineage.TargetMatrix.Targets = subset
	importArtifact(t, store, artifactOptions{ID: "preview_p1g_partial_loading", Lineage: loadingLineage})

	request := buildRequest(ws, matrixID, []CellRef{
		{Target: subset[0], State: ws.States[0], PreviewArtifactID: "preview_p1g_partial_default", PreviewRevision: 1},
		{Target: subset[0], State: ws.States[1], PreviewArtifactID: "preview_p1g_partial_loading", PreviewRevision: 1},
	})
	request.Targets = subset
	document := mustBuild(t, builder, request)

	if !sameStringSet(document.TargetMatrix.Targets, subset) {
		t.Fatalf("targets = %+v", document.TargetMatrix.Targets)
	}
	if len(document.Entries) != 2 || document.Coverage != (Coverage{Targets: 1, States: 2, Expected: 2, Completed: 2, Complete: true}) {
		t.Fatalf("entries = %d coverage = %+v", len(document.Entries), document.Coverage)
	}
}

func TestBuildMissingCellIsRecorded(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_missing"
	cells := fullMatrix(t, ws, store, matrixID)

	request := buildRequest(ws, matrixID, cells[1:])
	document := mustBuild(t, builder, request)

	missing := entryAt(t, document, ws.Targets[0], ws.States[0])
	if missing.Status != StatusMissing {
		t.Fatalf("cell %s/%s status = %s, want missing", missing.Target, missing.State, missing.Status)
	}
	if missing.PreviewArtifactID != nil || missing.PreviewRevision != nil || missing.PreviewRevisionSHA256 != nil ||
		missing.PreviewMode != nil || missing.Lineage != nil || missing.FallbackAsset != nil || missing.Error != nil {
		t.Fatalf("missing cell carries claims: %+v", missing)
	}
	if document.Coverage.Missing != 1 || document.Coverage.Completed != 3 || document.Coverage.Complete {
		t.Fatalf("coverage = %+v", document.Coverage)
	}
}

func TestBuildDeclaredFailureCell(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_declared"
	importArtifact(t, store, artifactOptions{ID: "preview_p1g_declared", Lineage: testLineage(ws, ws.Targets[0], "default", matrixID)})

	request := buildRequest(ws, matrixID, []CellRef{
		{Target: ws.Targets[0], State: "default", PreviewArtifactID: "preview_p1g_declared", PreviewRevision: 1},
		{Target: ws.Targets[1], State: "default", Error: &EntryError{Code: preview.LineageInvalid, Message: "layout revision was rejected"}},
	})
	document := mustBuild(t, builder, request)

	failed := entryAt(t, document, ws.Targets[1], "default")
	if failed.Status != StatusFailed || failed.Error == nil || failed.Error.Code != preview.LineageInvalid {
		t.Fatalf("failed cell = %+v", failed)
	}
	if failed.PreviewArtifactID != nil || failed.Lineage != nil || failed.FallbackAsset != nil {
		t.Fatalf("failed cell fabricated evidence: %+v", failed)
	}
	if document.Coverage.Failed != 1 || document.Coverage.Complete {
		t.Fatalf("coverage = %+v", document.Coverage)
	}
}

func TestBuildMissingPreviewArtifactIsFailed(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
	_, builder := newTestStore(t)
	const matrixID = "matrix_p1g_absent"
	request := buildRequest(ws, matrixID, []CellRef{
		{Target: ws.Targets[0], State: "default", PreviewArtifactID: "preview_p1g_absent", PreviewRevision: 1},
	})
	document := mustBuild(t, builder, request)

	entry := entryAt(t, document, ws.Targets[0], "default")
	if entry.Status != StatusFailed || entry.Error == nil || entry.Error.Code != string(preview.CodeArtifactMissing) {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.PreviewArtifactID != nil || entry.FallbackAsset != nil || entry.Lineage != nil {
		t.Fatalf("entry claims a preview that does not exist: %+v", entry)
	}
}

func TestBuildFailedPreviewArtifactIsFailedWithoutFallback(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_failed"
	importArtifact(t, store, artifactOptions{
		ID: "preview_p1g_failed", Lineage: testLineage(ws, ws.Targets[0], "default", matrixID),
		Status: preview.StatusFailed, ErrorCode: preview.CodeViewerFetchFailed, Message: "viewer bytes could not be fetched",
	})
	request := buildRequest(ws, matrixID, []CellRef{{
		Target: ws.Targets[0], State: "default", PreviewArtifactID: "preview_p1g_failed", PreviewRevision: 1,
	}})
	document := mustBuild(t, builder, request)

	entry := entryAt(t, document, ws.Targets[0], "default")
	if entry.Status != StatusFailed || entry.Error == nil || entry.Error.Code != string(preview.CodeViewerFetchFailed) {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Lineage != nil || entry.FallbackAsset != nil {
		t.Fatalf("failed cell fabricated lineage or fallback: %+v", entry)
	}
	if entry.PreviewArtifactID == nil || *entry.PreviewArtifactID != "preview_p1g_failed" {
		t.Fatalf("failed cell lost its preview identity: %+v", entry)
	}
}

func TestBuildExplorePreviewIsRejected(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_explore"
	importArtifact(t, store, artifactOptions{ID: "preview_p1g_explore", Mode: preview.ModeExplore})
	request := buildRequest(ws, matrixID, []CellRef{{
		Target: ws.Targets[0], State: "default", PreviewArtifactID: "preview_p1g_explore", PreviewRevision: 1,
	}})
	document := mustBuild(t, builder, request)

	entry := entryAt(t, document, ws.Targets[0], "default")
	if entry.Status != StatusFailed || entry.Error == nil || entry.Error.Code != string(preview.CodeInvalidArtifact) {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.FallbackAsset != nil || entry.Lineage != nil {
		t.Fatalf("explore preview leaked into completed fields: %+v", entry)
	}
}

func TestBuildLineageMismatchIsFailed(t *testing.T) {
	tests := []struct {
		name     string
		evidence bool
		mutate   func(*preview.Lineage)
	}{
		{"target impersonation", false, func(l *preview.Lineage) { l.Target = "iphone_portrait" }},
		{"state impersonation", false, func(l *preview.Lineage) { l.State = "loading" }},
		{"layout revision", false, func(l *preview.Lineage) { l.LayoutRevision = 2 }},
		{"layout digest", false, func(l *preview.Lineage) { l.LayoutRevisionSHA256 = "sha256:" + strings.Repeat("0", 64) }},
		{"source asset id", false, func(l *preview.Lineage) { l.SourceAssetID = "asset_other" }},
		{"source asset digest", false, func(l *preview.Lineage) { l.SourceAssetSHA256 = strings.Repeat("b", 64) }},
		{"matrix id", false, func(l *preview.Lineage) { l.TargetMatrix.ID = "matrix_p1g_other" }},
		{"matrix targets", false, func(l *preview.Lineage) {
			l.TargetMatrix.Targets = []string{"ipad_landscape", "iphone_portrait", "ipad_portrait"}
		}},
		{"evidence dropped", true, func(l *preview.Lineage) { l.EvidenceSHA256 = nil }},
		{"evidence digest", true, func(l *preview.Lineage) { wrong := strings.Repeat("c", 64); l.EvidenceSHA256 = &wrong }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := newWorkspace(t, workspaceOptions{States: []string{"default"}, Evidence: tc.evidence})
			store, builder := newTestStore(t)
			const matrixID = "matrix_p1g_mismatch"
			lineage := testLineage(ws, "ipad_landscape", "default", matrixID)
			tc.mutate(lineage)
			importArtifact(t, store, artifactOptions{ID: "preview_p1g_mismatch", Lineage: lineage})
			request := buildRequest(ws, matrixID, []CellRef{{
				Target: "ipad_landscape", State: "default", PreviewArtifactID: "preview_p1g_mismatch", PreviewRevision: 1,
			}})
			document := mustBuild(t, builder, request)
			entry := entryAt(t, document, "ipad_landscape", "default")
			if entry.Status != StatusFailed || entry.Error == nil || entry.Error.Code != preview.LineageMismatch {
				t.Fatalf("entry = %+v", entry)
			}
			if entry.Lineage != nil || entry.FallbackAsset != nil {
				t.Fatalf("mismatched lineage leaked into the matrix: %+v", entry)
			}
		})
	}
}

func TestBuildEvidenceBoundMatrix(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{States: []string{"default"}, Evidence: true})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_evidence"
	importArtifact(t, store, artifactOptions{ID: "preview_p1g_evidence", Lineage: testLineage(ws, ws.Targets[0], "default", matrixID)})
	request := buildRequest(ws, matrixID, []CellRef{{
		Target: ws.Targets[0], State: "default", PreviewArtifactID: "preview_p1g_evidence", PreviewRevision: 1,
	}})
	document := mustBuild(t, builder, request)

	if document.EvidenceSHA256 == nil || *document.EvidenceSHA256 != ws.EvidenceSHA {
		t.Fatalf("matrix evidence = %v, want %s", document.EvidenceSHA256, ws.EvidenceSHA)
	}
	entry := entryAt(t, document, ws.Targets[0], "default")
	if entry.Status != StatusCompleted || entry.Lineage == nil || entry.Lineage.EvidenceSHA256 == nil {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestBuildRequestRejections(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_reject"
	importArtifact(t, store, artifactOptions{ID: "preview_p1g_reject", Lineage: testLineage(ws, ws.Targets[0], "default", matrixID)})
	valid := CellRef{Target: ws.Targets[0], State: "default", PreviewArtifactID: "preview_p1g_reject", PreviewRevision: 1}

	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr string
	}{
		{"unknown target", func(r *Request) { r.Targets = []string{"watch_small"} }, CodeInvalid},
		{"unknown state", func(r *Request) { r.States = []string{"empty"} }, CodeInvalid},
		{"duplicate target selection", func(r *Request) {
			r.Targets = []string{"iphone_portrait", "iphone_portrait"}
			r.Cells = append(r.Cells, CellRef{Target: ws.Targets[1], State: "default", PreviewArtifactID: "preview_p1g_reject", PreviewRevision: 1})
		}, CodeInvalid},
		{"cell outside selection", func(r *Request) {
			r.Targets = []string{"ipad_landscape"}
			r.Cells = []CellRef{{Target: "iphone_portrait", State: "default", PreviewArtifactID: "preview_p1g_reject", PreviewRevision: 1}}
		}, CodeInvalid},
		{"duplicate cell", func(r *Request) { r.Cells = append(r.Cells, valid) }, CodeInvalid},
		{"duplicate artifact across cells", func(r *Request) {
			r.Cells = append(r.Cells, CellRef{Target: ws.Targets[1], State: "default", PreviewArtifactID: "preview_p1g_reject", PreviewRevision: 1})
		}, CodeInvalid},
		{"revision without artifact", func(r *Request) {
			r.Cells = []CellRef{{Target: ws.Targets[0], State: "default", PreviewRevision: 1}}
		}, CodeInvalid},
		{"failure with preview", func(r *Request) {
			r.Cells[0].Error = &EntryError{Code: preview.LineageInvalid, Message: "rejected"}
		}, CodeInvalid},
		{"unknown error code", func(r *Request) {
			r.Cells[0] = CellRef{Target: ws.Targets[0], State: "default", Error: &EntryError{Code: "made_up", Message: "rejected"}}
		}, CodeInvalid},
		{"empty error message", func(r *Request) {
			r.Cells[0] = CellRef{Target: ws.Targets[0], State: "default", Error: &EntryError{Code: preview.LineageInvalid, Message: " "}}
		}, CodeInvalid},
		{"matrix id", func(r *Request) { r.MatrixID = "preview_p1g" }, CodeInvalid},
		{"session id", func(r *Request) { r.SessionID = "" }, CodeInvalid},
		{"workspace", func(r *Request) { r.WorkspaceRoot = "" }, CodeInvalid},
		{"layout revision", func(r *Request) { r.Layout.Revision = 0 }, CodeInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := buildRequest(ws, matrixID, []CellRef{valid})
			tc.mutate(&request)
			mustFailBuild(t, builder, request, tc.wantErr)
		})
	}
}

func TestBuildRequiresStore(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
	_, err := NewBuilder(nil).Build(context.Background(), buildRequest(ws, "matrix_p1g_store", nil))
	if err == nil || ErrorCode(err) != CodeStoreUnavailable {
		t.Fatalf("nil store error = %v", err)
	}
}

func rewriteLayout(t *testing.T, root string, mutate func(map[string]any)) {
	t.Helper()
	dir := filepath.Join(root, ".codeagent", "assets", "ui-layout", "layout_p1g", "revision-000001")
	layoutBytes, err := os.ReadFile(filepath.Join(dir, "layout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(layoutBytes, &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	unsigned, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "layout.json"), unsigned, 0o600); err != nil {
		t.Fatal(err)
	}
	metadataBytes, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	digest := sha256Hex(unsigned)
	metadata["layout_sha256"] = digest
	metadata["layout_revision_sha256"] = "sha256:" + digest
	rewritten, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildLayoutFailures(t *testing.T) {
	t.Run("missing layout revision", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
		store, builder := newTestStore(t)
		request := buildRequest(ws, "matrix_p1g_layout", nil)
		request.Layout.Revision = 7
		mustFailBuild(t, builder, request, CodeLayoutUnavailable)
		_ = store
	})

	t.Run("tampered layout bytes", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
		_, builder := newTestStore(t)
		dir := filepath.Join(ws.Root, ".codeagent", "assets", "ui-layout", "layout_p1g", "revision-000001")
		file, err := os.OpenFile(filepath.Join(dir, "layout.json"), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("\n"); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		mustFailBuild(t, builder, buildRequest(ws, "matrix_p1g_layout", nil), CodeLayoutMismatch)
	})

	t.Run("layout identity mismatch", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
		_, builder := newTestStore(t)
		rewriteMetadata(t, ws.Root, func(metadata map[string]any) {
			metadata["layout_artifact_id"] = "layout_other"
		})
		mustFailBuild(t, builder, buildRequest(ws, "matrix_p1g_layout", nil), CodeLayoutMismatch)
	})

	t.Run("source identity mismatch", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
		_, builder := newTestStore(t)
		rewriteLayout(t, ws.Root, func(document map[string]any) {
			document["source"].(map[string]any)["asset_id"] = "asset_other"
		})
		mustFailBuild(t, builder, buildRequest(ws, "matrix_p1g_layout", nil), CodeLayoutMismatch)
	})

	t.Run("invalid layout document", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{States: []string{"default"}})
		_, builder := newTestStore(t)
		rewriteLayout(t, ws.Root, func(document map[string]any) {
			delete(document, "screen")
		})
		mustFailBuild(t, builder, buildRequest(ws, "matrix_p1g_layout", nil), CodeLayoutInvalid)
	})

	t.Run("unsupported layout target", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{Targets: []string{"iphone_portrait"}, States: []string{"default"}})
		_, builder := newTestStore(t)
		rewriteLayout(t, ws.Root, func(document map[string]any) {
			document["responsive"].(map[string]any)["targets"].(map[string]any)["watch_small"] = map[string]any{"columns": 1}
		})
		mustFailBuild(t, builder, buildRequest(ws, "matrix_p1g_layout", nil), CodeLayoutInvalid)
	})
}

func rewriteMetadata(t *testing.T, root string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(root, ".codeagent", "assets", "ui-layout", "layout_p1g", "revision-000001", "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	mutate(metadata)
	rewritten, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildReplayIsStable(t *testing.T) {
	ws := newWorkspace(t, workspaceOptions{})
	store, builder := newTestStore(t)
	const matrixID = "matrix_p1g_replay"
	cells := fullMatrix(t, ws, store, matrixID)

	first := mustBuild(t, builder, buildRequest(ws, matrixID, cells))
	second := mustBuild(t, builder, buildRequest(ws, matrixID, cells))
	if first.MatrixSHA256 != second.MatrixSHA256 {
		t.Fatalf("digests differ: %s vs %s", first.MatrixSHA256, second.MatrixSHA256)
	}
	firstBytes, err := Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("replayed matrix bytes differ")
	}
	firstManifest, err := BuildManifest(first).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	secondManifest, err := BuildManifest(second).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(firstManifest) != string(secondManifest) {
		t.Fatalf("replayed manifest bytes differ")
	}
}
