package previewmatrix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/preview"
	"code-agent/internal/preview/matrix"
	"code-agent/internal/tools"
	previewtool "code-agent/internal/tools/preview"
	"code-agent/internal/uicontract"
	"code-agent/schemas"
)

const (
	testSession = "session_p1g_tool"
	testTurn    = "turn_p1g_tool"
	testMatrix  = "matrix_p1g_tool"
)

var testTime = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

type fixture struct {
	Root          string
	Store         *preview.FSStore
	StoreBase     string
	LayoutID      string
	LayoutRev     int64
	LayoutSHA     string
	SourceAssetID string
	SourceSHA     string
	Targets       []string
	States        []string
}

func newFixture(t *testing.T, targets, states []string) fixture {
	t.Helper()
	root := t.TempDir()
	sourcePNG := drawPNG(t, 200, 400)
	if err := os.WriteFile(filepath.Join(root, "source.png"), sourcePNG, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceSHA := shaHex(sourcePNG)

	sorted := append([]string(nil), targets...)
	if len(sorted) == 0 {
		sorted = []string{"ipad_landscape", "iphone_portrait"}
	}
	if len(states) == 0 {
		states = []string{"default", "loading"}
	}
	targetMap := make(map[string]any, len(sorted))
	for _, target := range sorted {
		targetMap[target] = map[string]any{"columns": 1}
	}
	stateList := make([]any, 0, len(states))
	for _, state := range states {
		stateList = append(stateList, state)
	}
	layout := map[string]any{
		"schema": "ui_layout.v1",
		"source": map[string]any{
			"asset_id":            "asset_p1g_tool_source",
			"source_asset_sha256": sourceSHA,
			"canvas":              map[string]any{"width": 200, "height": 400},
		},
		"screen": map[string]any{
			"type":             "fixture_detail",
			"primary_action":   "Continue",
			"content_priority": []any{"title"},
			"states":           stateList,
		},
		"root": map[string]any{
			"id":           "n_root",
			"role":         "screen",
			"bbox":         map[string]any{"x": 0.0, "y": 0.0, "width": 1.0, "height": 1.0},
			"layout":       map[string]any{"mode": "vertical_stack"},
			"children":     []any{},
			"evidence_ids": []any{},
			"confidence":   0.9,
			"evidence":     "inferred",
		},
		"responsive": map[string]any{
			"invariants":    []any{map[string]any{"id": "inv_action", "statement": "The primary action remains visible."}},
			"targets":       targetMap,
			"node_policies": []any{},
		},
		"assumptions": []any{},
		"confidence":  0.9,
	}
	layoutBytes, err := json.Marshal(layout)
	if err != nil {
		t.Fatal(err)
	}
	layoutSHA := shaHex(layoutBytes)
	metadata := map[string]any{
		"kind":                   "ui_layout_revision",
		"layout_artifact_id":     "layout_p1g_tool",
		"layout_revision":        1,
		"layout_sha256":          layoutSHA,
		"layout_revision_sha256": "sha256:" + layoutSHA,
		"source_path":            "source.png",
		"source_asset_id":        "asset_p1g_tool_source",
		"source_asset_sha256":    sourceSHA,
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	revisionDir := filepath.Join(root, ".codeagent", "assets", "ui-layout", "layout_p1g_tool", "revision-000001")
	if err := os.MkdirAll(revisionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(revisionDir, "layout.json"), layoutBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(revisionDir, "metadata.json"), metadataBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	storeBase := t.TempDir()
	store, err := preview.NewFSStore(storeBase)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		Root:          root,
		Store:         store,
		StoreBase:     storeBase,
		LayoutID:      "layout_p1g_tool",
		LayoutRev:     1,
		LayoutSHA:     "sha256:" + layoutSHA,
		SourceAssetID: "asset_p1g_tool_source",
		SourceSHA:     sourceSHA,
		Targets:       append([]string(nil), sorted...),
		States:        append([]string(nil), states...),
	}
}

func drawPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 80, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func shaHex(data []byte) string {
	return strings.TrimPrefix(preview.DigestBytes(data), "sha256:")
}

func (f fixture) lineage(target, state, matrixID string) *preview.Lineage {
	return &preview.Lineage{
		Schema:               preview.LineageSchema,
		LayoutArtifactID:     f.LayoutID,
		LayoutRevision:       f.LayoutRev,
		LayoutRevisionSHA256: f.LayoutSHA,
		SourceAssetID:        f.SourceAssetID,
		SourceAssetSHA256:    f.SourceSHA,
		Approval:             preview.LineageApproval{State: "not_required"},
		Target:               target,
		State:                state,
		TargetMatrix:         preview.LineageTargetMatrix{ID: matrixID, Targets: f.Targets},
	}
}

type artifactOptions struct {
	Key      string
	ID       string
	Revision int64
	Lineage  *preview.Lineage
	Status   preview.Status
	Code     preview.ErrorCode
	Message  string
	Session  string
}

func (f fixture) importArtifact(t *testing.T, opts artifactOptions) preview.Artifact {
	t.Helper()
	session := opts.Session
	if session == "" {
		session = testSession
	}
	revision := opts.Revision
	if revision == 0 {
		revision = 1
	}
	status := opts.Status
	if status == "" {
		status = preview.StatusReady
	}
	artifactID := opts.ID
	namespace := preview.Namespace{
		SessionID:   session,
		Capability:  previewtool.Capability,
		ProducerID:  previewProducerID,
		ArtifactKey: opts.Key,
	}
	if artifactID == "" {
		artifactID = preview.ArtifactID(namespace)
	}
	if opts.Key == "" {
		namespace = preview.Namespace{SessionID: session}
	}
	fallback := drawPNG(t, 40, 30)
	config, err := png.DecodeConfig(bytes.NewReader(fallback))
	if err != nil {
		t.Fatal(err)
	}
	const assetID = "asset_p1g_tool_fallback"
	artifact := preview.Artifact{
		SchemaVersion: preview.SchemaVersion,
		ArtifactID:    artifactID,
		Revision:      revision,
		Status:        status,
		Kind:          preview.KindInteractiveHTML,
		Title:         "P1-G tool preview",
		Capability:    previewtool.Capability,
		ProducerID:    previewProducerID,
		SessionID:     session,
		TurnID:        testTurn,
		CallID:        "call_p1g_tool",
		CreatedAt:     testTime,
		UpdatedAt:     testTime,
		Presentation:  preview.DefaultPresentation(),
		Assets: []preview.Asset{{
			AssetID:               assetID,
			Role:                  preview.RoleFallback,
			MIMEType:              "image/png",
			SizeBytes:             int64(len(fallback)),
			SHA256:                preview.DigestBytes(fallback),
			ContentURL:            preview.AssetURL(session, artifactID, revision, assetID),
			Width:                 config.Width,
			Height:                config.Height,
			WorkspaceRelativePath: "fallback.png",
		}},
		Fallback: &preview.Fallback{AssetID: assetID, Text: "Static evidence"},
		Security: preview.DefaultSecurity(),
		Metadata: map[string]any{preview.MetadataPreviewMode: string(preview.ModeAdaptation)},
	}
	if opts.Lineage != nil {
		artifact.Metadata[preview.MetadataLineage] = opts.Lineage
	}
	switch status {
	case preview.StatusFailed, preview.StatusUnavailable:
		code := opts.Code
		if code == "" {
			code = preview.CodeRenderFailed
		}
		message := opts.Message
		if message == "" {
			message = "Preview rendering failed."
		}
		artifact.Error = &preview.Error{Code: code, Message: message, Retryable: true}
	default:
		viewer := []byte("<html><body><div id=\"p1g\">preview</div></body></html>")
		artifact.Viewer = &preview.Viewer{
			URL:       preview.ViewerURL(session, artifactID, revision),
			MIMEType:  preview.ViewerMIMEType,
			SizeBytes: int64(len(viewer)),
			SHA256:    preview.DigestBytes(viewer),
		}
		digest, err := preview.RevisionSHA256(artifact)
		if err != nil {
			t.Fatal(err)
		}
		artifact.RevisionSHA256 = digest
		if err := artifact.Validate(); err != nil {
			t.Fatalf("fixture artifact is invalid: %v", err)
		}
		if err := f.Store.ImportArtifact(context.Background(), namespace, artifact, viewer,
			map[string][]byte{assetID: fallback}); err != nil {
			t.Fatalf("import artifact: %v", err)
		}
		return artifact
	}
	digest, err := preview.RevisionSHA256(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifact.RevisionSHA256 = digest
	if err := artifact.Validate(); err != nil {
		t.Fatalf("fixture artifact is invalid: %v", err)
	}
	if err := f.Store.ImportArtifact(context.Background(), namespace, artifact, nil,
		map[string][]byte{assetID: fallback}); err != nil {
		t.Fatalf("import artifact: %v", err)
	}
	return artifact
}

func (f fixture) baseInput(matrixID string) map[string]any {
	return map[string]any{
		"matrix_id": matrixID,
		"layout": map[string]any{
			"artifact_id":     f.LayoutID,
			"revision":        f.LayoutRev,
			"revision_sha256": f.LayoutSHA,
		},
		"target_matrix": map[string]any{
			"id":      matrixID,
			"targets": append([]string(nil), f.Targets...),
		},
		"states":  append([]string(nil), f.States...),
		"entries": []any{},
	}
}

func execute(t *testing.T, fx fixture, input map[string]any) tools.ToolResult {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: fx.Root,
		SessionID:     testSession,
		TurnID:        testTurn,
		CallID:        "call_p1g_matrix",
		PreviewStore:  fx.Store,
	}, raw)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return result
}

func decodeResult(t *testing.T, res tools.ToolResult) result {
	t.Helper()
	if res.Failure != nil {
		t.Fatalf("tool failure: %+v", res.Failure)
	}
	var out result
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if err := uicontract.ValidateAgainstSchema(schemas.UIPreviewMatrixResultV1, res.Output); err != nil {
		t.Fatalf("result schema: %v", err)
	}
	return out
}

func keyRef(key string) map[string]any { return map[string]any{"artifact_key": key} }

func idRef(id string, revision int64) map[string]any {
	return map[string]any{"artifact_id": id, "revision": revision}
}

func TestExecuteBuildsMatrixFromControlledRefs(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_positive"
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
	result := execute(t, fx, input)
	out := decodeResult(t, result)

	if out.MatrixID != matrixID || out.MatrixRevision != 1 || !out.Created {
		t.Fatalf("matrix identity = %+v", out)
	}
	if out.Coverage != (matrix.Coverage{Targets: 2, States: 2, Expected: 4, Completed: 2, Failed: 1, Missing: 1}) {
		t.Fatalf("coverage = %+v", out.Coverage)
	}
	if len(out.Entries) != 4 {
		t.Fatalf("entries = %d", len(out.Entries))
	}
	completed := entryOf(t, out, "ipad_landscape", "default")
	if completed.Status != matrix.StatusCompleted || completed.PreviewRef == nil || completed.FallbackAsset == nil {
		t.Fatalf("completed entry = %+v", completed)
	}
	if got := fx.Store; got == nil {
		t.Fatal("store missing")
	}
	resolved, err := fx.Store.GetArtifact(context.Background(), testSession, completed.PreviewRef.ArtifactID, completed.PreviewRef.Revision)
	if err != nil {
		t.Fatalf("resolve recorded ref: %v", err)
	}
	if resolved.RevisionSHA256 != completed.PreviewRef.RevisionSHA256 {
		t.Fatalf("recorded digest = %s, want %s", completed.PreviewRef.RevisionSHA256, resolved.RevisionSHA256)
	}
	failed := entryOf(t, out, "ipad_landscape", "loading")
	if failed.Status != matrix.StatusFailed || failed.Error == nil || failed.Error.Code != string(preview.CodeViewerFetchFailed) {
		t.Fatalf("failed entry = %+v", failed)
	}
	if failed.PreviewRef == nil {
		t.Fatal("failed preview revision must keep its recorded identity")
	}
	missing := entryOf(t, out, "iphone_portrait", "loading")
	if missing.Status != matrix.StatusMissing || missing.PreviewRef != nil || missing.Error != nil || missing.FallbackAsset != nil {
		t.Fatalf("missing entry = %+v", missing)
	}
	if len(out.ErrorTypes) != 1 || out.ErrorTypes[0] != string(preview.CodeViewerFetchFailed) {
		t.Fatalf("error types = %v", out.ErrorTypes)
	}

	persisted := readPersistedMatrix(t, fx, out)
	if persisted.MatrixSHA256 != out.MatrixSHA256 || persisted.Coverage != out.Coverage {
		t.Fatalf("persisted matrix does not match the result")
	}
	manifest := readPersistedManifest(t, fx, out)
	if manifest.MatrixSHA256 != out.MatrixSHA256 || manifest.Schema != out.ManifestSchema {
		t.Fatalf("manifest does not match the result")
	}

	second := decodeResult(t, execute(t, fx, input))
	if second.Created || second.MatrixRevision != out.MatrixRevision || second.MatrixSHA256 != out.MatrixSHA256 {
		t.Fatalf("repeat call was not idempotent: %+v", second)
	}
}

func TestExecuteAcceptsExplicitArtifactRef(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_explicit"
	artifact := fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:ipad-default", Lineage: fx.lineage("ipad_landscape", "default", matrixID),
	})
	input := fx.baseInput(matrixID)
	input["entries"] = []any{
		map[string]any{"target": "ipad_landscape", "state": "default", "preview_ref": idRef(artifact.ArtifactID, artifact.Revision)},
	}
	out := decodeResult(t, execute(t, fx, input))
	entry := entryOf(t, out, "ipad_landscape", "default")
	if out.Coverage.Completed != 1 || entry.PreviewRef == nil || entry.PreviewRef.ArtifactID != artifact.ArtifactID {
		t.Fatalf("explicit ref was not resolved: %+v", entry)
	}
}

func TestExecuteRejectsTargetMatrixMismatch(t *testing.T) {
	fx := newFixture(t, nil, nil)
	input := fx.baseInput(testMatrix + "_mismatch")
	input["target_matrix"].(map[string]any)["id"] = "matrix_something_else"
	res := execute(t, fx, input)
	if res.Failure == nil || res.Failure.Kind != "invalid_arguments" {
		t.Fatalf("failure = %+v", res.Failure)
	}
}

func TestExecuteRejectsSharedArtifact(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_shared"
	fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:shared", Lineage: fx.lineage("ipad_landscape", "default", matrixID),
	})
	input := fx.baseInput(matrixID)
	input["entries"] = []any{
		map[string]any{"target": "ipad_landscape", "state": "default", "preview_ref": keyRef("ui-adaptation:shared")},
		map[string]any{"target": "ipad_landscape", "state": "loading", "preview_ref": keyRef("ui-adaptation:shared")},
	}
	res := execute(t, fx, input)
	if res.Failure == nil || res.Failure.Kind != "invalid_arguments" {
		t.Fatalf("failure = %+v", res.Failure)
	}
	if !strings.Contains(res.Content, "more than one cell") {
		t.Fatalf("failure content = %q", res.Content)
	}
}

func TestExecuteRejectsLayoutDigestMismatch(t *testing.T) {
	fx := newFixture(t, nil, nil)
	input := fx.baseInput(testMatrix + "_layout")
	input["layout"].(map[string]any)["revision_sha256"] = "sha256:" + strings.Repeat("a", 64)
	res := execute(t, fx, input)
	if res.Failure == nil || res.Failure.Kind != "invalid_arguments" {
		t.Fatalf("failure = %+v", res.Failure)
	}
	if _, err := os.Stat(filepath.Join(fx.Root, ".codeagent", "assets", "ui-preview-matrix")); err == nil {
		t.Fatal("a rejected layout digest must not persist a matrix")
	}
}

func TestExecuteRejectsMalformedReferences(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_malformed"
	for name, mutate := range map[string]func(map[string]any){
		"absolute path": func(input map[string]any) {
			input["entries"] = []any{map[string]any{
				"target": "ipad_landscape", "state": "default",
				"preview_ref": map[string]any{"artifact_id": "/Users/me/preview", "revision": 1},
			}}
		},
		"artifact id without revision": func(input map[string]any) {
			input["entries"] = []any{map[string]any{
				"target": "ipad_landscape", "state": "default",
				"preview_ref": map[string]any{"artifact_id": "preview_abc"},
			}}
		},
		"both key and id": func(input map[string]any) {
			input["entries"] = []any{map[string]any{
				"target": "ipad_landscape", "state": "default",
				"preview_ref": map[string]any{"artifact_key": "k", "artifact_id": "preview_abc", "revision": 1},
			}}
		},
		"neither key nor id": func(input map[string]any) {
			input["entries"] = []any{map[string]any{
				"target": "ipad_landscape", "state": "default", "preview_ref": map[string]any{},
			}}
		},
		"bad digest": func(input map[string]any) {
			input["entries"] = []any{map[string]any{
				"target": "ipad_landscape", "state": "default",
				"preview_ref": map[string]any{"artifact_key": "k", "revision_sha256": "sha256:nothex"},
			}}
		},
		"ref and error": func(input map[string]any) {
			input["entries"] = []any{map[string]any{
				"target": "ipad_landscape", "state": "default",
				"preview_ref": keyRef("k"),
				"error":       map[string]any{"code": "preview_render_failed", "message": "x"},
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := fx.baseInput(matrixID)
			mutate(input)
			res := execute(t, fx, input)
			if res.Failure == nil || res.Failure.Kind != "invalid_arguments" {
				t.Fatalf("failure = %+v", res.Failure)
			}
		})
	}
}

func TestExecuteRejectsCellOutsideMatrix(t *testing.T) {
	fx := newFixture(t, nil, nil)
	input := fx.baseInput(testMatrix + "_cell")
	input["entries"] = []any{
		map[string]any{"target": "duo_unfolded", "state": "default"},
	}
	res := execute(t, fx, input)
	if res.Failure == nil || res.Failure.Kind != "invalid_arguments" {
		t.Fatalf("failure = %+v", res.Failure)
	}
}

func TestExecuteRecordsMissingRevisionAsFailedCell(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_missingrev"
	fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:ipad-default", Lineage: fx.lineage("ipad_landscape", "default", matrixID),
	})
	input := fx.baseInput(matrixID)
	input["entries"] = []any{
		map[string]any{"target": "ipad_landscape", "state": "default", "preview_ref": map[string]any{
			"artifact_key": "ui-adaptation:ipad-default", "revision": 7,
		}},
	}
	out := decodeResult(t, execute(t, fx, input))
	entry := entryOf(t, out, "ipad_landscape", "default")
	if out.Coverage.Failed != 1 || entry.Error == nil || entry.Error.Code != string(preview.CodeArtifactMissing) {
		t.Fatalf("missing revision entry = %+v", entry)
	}
}

func TestExecuteRecordsCorruptRevisionAsFailedCell(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_corrupt"
	artifact := fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:ipad-default", Lineage: fx.lineage("ipad_landscape", "default", matrixID),
	})
	sum := sha256.Sum256([]byte(testSession))
	artifactPath := filepath.Join(fx.StoreBase, "session_"+hex.EncodeToString(sum[:])[:24],
		artifact.ArtifactID, "1", "artifact.json")
	if err := os.WriteFile(artifactPath, []byte("{\"schema_version\":\"tampered\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := fx.baseInput(matrixID)
	input["entries"] = []any{
		map[string]any{"target": "ipad_landscape", "state": "default", "preview_ref": keyRef("ui-adaptation:ipad-default")},
	}
	out := decodeResult(t, execute(t, fx, input))
	entry := entryOf(t, out, "ipad_landscape", "default")
	if out.Coverage.Failed != 1 || entry.Error == nil {
		t.Fatalf("corrupt revision entry = %+v", entry)
	}
	if entry.Error.Code != string(preview.CodeInvalidArtifact) && entry.Error.Code != string(preview.CodeStoreUnavailable) {
		t.Fatalf("corrupt revision error = %+v", entry.Error)
	}
}

func TestExecuteRecordsCrossSessionReferenceAsFailedCell(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_cross"
	foreign := fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:foreign", Session: "session_other", Lineage: fx.lineage("ipad_landscape", "default", matrixID),
	})
	input := fx.baseInput(matrixID)
	input["entries"] = []any{
		map[string]any{"target": "ipad_landscape", "state": "default", "preview_ref": idRef(foreign.ArtifactID, foreign.Revision)},
	}
	out := decodeResult(t, execute(t, fx, input))
	entry := entryOf(t, out, "ipad_landscape", "default")
	if out.Coverage.Failed != 1 || entry.Status != matrix.StatusFailed || entry.Error == nil ||
		entry.Error.Code != string(preview.CodeArtifactMissing) {
		t.Fatalf("cross-session reference = %+v", entry)
	}
}

func TestExecuteRecordsWrongLineageAsFailedCell(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_lineage"
	fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:wrong-target", Lineage: fx.lineage("ipad_landscape", "default", matrixID),
	})
	input := fx.baseInput(matrixID)
	input["entries"] = []any{
		map[string]any{"target": "iphone_portrait", "state": "default", "preview_ref": keyRef("ui-adaptation:wrong-target")},
	}
	out := decodeResult(t, execute(t, fx, input))
	entry := entryOf(t, out, "iphone_portrait", "default")
	if out.Coverage.Failed != 1 || entry.Error == nil || entry.Error.Code != preview.LineageMismatch {
		t.Fatalf("wrong lineage entry = %+v", entry)
	}
}

func TestExecuteRejectsUnknownFields(t *testing.T) {
	fx := newFixture(t, nil, nil)
	input := fx.baseInput(testMatrix + "_unknown")
	input["workspace_root"] = "/tmp/elsewhere"
	res := execute(t, fx, input)
	if res.Failure == nil || res.Failure.Kind != "invalid_arguments" {
		t.Fatalf("failure = %+v", res.Failure)
	}
}

func TestExecuteWithoutPreviewStore(t *testing.T) {
	fx := newFixture(t, nil, nil)
	input := fx.baseInput(testMatrix + "_nostore")
	raw, _ := json.Marshal(input)
	res, err := NewTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: fx.Root, SessionID: testSession, TurnID: testTurn, CallID: "call",
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failure == nil || res.Failure.Kind != "unavailable" {
		t.Fatalf("failure = %+v", res.Failure)
	}
}

func TestModelVisibleOutputIsSanitizedAndActionable(t *testing.T) {
	fx := newFixture(t, nil, nil)
	matrixID := testMatrix + "_output"
	artifact := fx.importArtifact(t, artifactOptions{
		Key: "ui-adaptation:ipad-default", Lineage: fx.lineage("ipad_landscape", "default", matrixID),
	})
	input := fx.baseInput(matrixID)
	input["entries"] = []any{
		map[string]any{"target": "ipad_landscape", "state": "default", "preview_ref": keyRef("ui-adaptation:ipad-default")},
	}
	res := execute(t, fx, input)
	out := decodeResult(t, res)

	structured, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	visible := res.Content + "\n" + string(structured)
	for _, forbidden := range []string{fx.Root, "/Users/", "file://", "Bearer ", `"token"`, `"secret"`} {
		if strings.Contains(visible, forbidden) {
			t.Fatalf("model-visible output leaks %q: %s", forbidden, visible)
		}
	}
	// The agent loop commits Content, not Output, into the model transcript, so
	// the text channel itself must carry everything a follow-up call needs.
	if !strings.Contains(res.Content, artifact.ArtifactID) ||
		!strings.Contains(res.Content, artifact.RevisionSHA256) ||
		!strings.Contains(res.Content, out.MatrixPath) {
		t.Fatalf("model-visible content lacks the resolved reference: %s", res.Content)
	}
	entry := entryOf(t, out, "ipad_landscape", "default")
	if entry.PreviewRef == nil || entry.PreviewRef.ArtifactID == "" || entry.PreviewRef.Revision < 1 || entry.PreviewRef.RevisionSHA256 == "" {
		t.Fatalf("completed cell has no actionable preview reference: %+v", entry)
	}
	if entry.FallbackAsset == nil || entry.FallbackAsset.AssetID == "" || entry.FallbackAsset.SHA256 == "" {
		t.Fatalf("completed cell has no fallback asset identity: %+v", entry)
	}
	if !strings.Contains(res.Content, entry.FallbackAsset.AssetID) {
		t.Fatalf("model-visible content lacks the fallback asset identity: %s", res.Content)
	}
}

func TestToolBindingAndSchemas(t *testing.T) {
	tool := NewTool()
	binding := tool.ToolBinding()
	if binding.Capability != Capability || binding.ModelVisibility != tools.VisibilityDirect || binding.Executor != tools.ExecutorServer {
		t.Fatalf("binding = %+v", binding)
	}
	if !json.Valid(tool.InputSchema()) || !json.Valid(tool.OutputSchema()) {
		t.Fatal("tool schemas are not valid JSON")
	}
	registry := tools.NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	registered, ok := registry.Binding("ui_preview_matrix")
	if !ok || registered.Capability != Capability {
		t.Fatalf("registered binding = %+v", registered)
	}
}

func TestInputSchemaValidatesModelArguments(t *testing.T) {
	fx := newFixture(t, nil, nil)
	valid, err := json.Marshal(fx.baseInput(testMatrix + "_schema"))
	if err != nil {
		t.Fatal(err)
	}
	if failure := tools.ValidateInput(NewTool(), valid); failure != nil {
		t.Fatalf("valid input rejected: %+v", failure)
	}
	withUnknown := fx.baseInput(testMatrix + "_schema")
	withUnknown["workspace_root"] = "/tmp/elsewhere"
	unknown, err := json.Marshal(withUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if failure := tools.ValidateInput(NewTool(), unknown); failure == nil {
		t.Fatal("unknown field was accepted by the input schema")
	}
	missing := map[string]any{}
	raw, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if failure := tools.ValidateInput(NewTool(), raw); failure == nil {
		t.Fatal("missing required fields were accepted by the input schema")
	}
}

func entryOf(t *testing.T, out result, target, state string) resultEntry {
	t.Helper()
	for _, entry := range out.Entries {
		if entry.Target == target && entry.State == state {
			return entry
		}
	}
	t.Fatalf("result has no entry for %s/%s", target, state)
	return resultEntry{}
}

func readPersistedMatrix(t *testing.T, fx fixture, out result) matrix.Matrix {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fx.Root, filepath.FromSlash(out.MatrixPath)))
	if err != nil {
		t.Fatalf("read persisted matrix: %v", err)
	}
	document, err := matrix.Decode(data)
	if err != nil {
		t.Fatalf("decode persisted matrix: %v", err)
	}
	digest, err := document.Digest()
	if err != nil || digest != out.MatrixSHA256 {
		t.Fatalf("persisted digest = %s (%v), want %s", digest, err, out.MatrixSHA256)
	}
	return document
}

func readPersistedManifest(t *testing.T, fx fixture, out result) matrix.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fx.Root, filepath.FromSlash(out.ManifestPath)))
	if err != nil {
		t.Fatalf("read persisted manifest: %v", err)
	}
	var manifest matrix.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if got := preview.DigestBytes(data); got != out.ManifestSHA256 {
		t.Fatalf("manifest digest = %s, want %s", got, out.ManifestSHA256)
	}
	return manifest
}
