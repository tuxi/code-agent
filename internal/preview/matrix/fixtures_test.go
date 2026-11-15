package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/preview"
)

const testSession = "session_p1g"

var testTime = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

var (
	defaultTargets = []string{"ipad_landscape", "iphone_portrait"}
	defaultStates  = []string{"default", "loading"}
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sha256Hex(data []byte) string {
	return strings.TrimPrefix(preview.DigestBytes(data), "sha256:")
}

type workspaceOptions struct {
	Targets  []string
	States   []string
	Evidence bool
}

type testWorkspace struct {
	Root          string
	Layout        LayoutRef
	Identity      LayoutIdentity
	Source        SourceIdentity
	EvidenceSHA   string
	Targets       []string
	States        []string
	SourceSHA     string
	FallbackBytes []byte
}

func newWorkspace(t *testing.T, opts workspaceOptions) testWorkspace {
	t.Helper()
	targets := opts.Targets
	if len(targets) == 0 {
		targets = defaultTargets
	}
	states := opts.States
	if len(states) == 0 {
		states = defaultStates
	}
	root := t.TempDir()
	sourceBytes := readFixture(t, "source.png")
	if err := os.WriteFile(filepath.Join(root, "source.png"), sourceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceSHA := sha256Hex(sourceBytes)

	targetMap := make(map[string]any, len(targets))
	stateList := make([]any, 0, len(states))
	sortedTargets := append([]string(nil), targets...)
	sortStrings(sortedTargets)
	for _, target := range targets {
		targetMap[target] = map[string]any{"columns": 1}
	}
	for _, state := range states {
		stateList = append(stateList, state)
	}
	source := map[string]any{
		"asset_id":            "asset_p1g_source",
		"source_asset_sha256": sourceSHA,
		"canvas":              map[string]any{"width": 200, "height": 400},
	}
	metadata := map[string]any{
		"kind":                   "ui_layout_revision",
		"layout_artifact_id":     "layout_p1g",
		"layout_revision":        1,
		"source_path":            "source.png",
		"source_asset_id":        "asset_p1g_source",
		"source_asset_sha256":    sourceSHA,
		"canvas":                 map[string]any{"width": 200, "height": 400},
		"node_count":             1,
		"assumption_count":       0,
		"overlay_width":          200,
		"overlay_height":         400,
		"source_mime_type":       "image/png",
		"layout_revision_sha256": "",
	}
	evidenceSHA := ""
	if opts.Evidence {
		evidenceBytes := []byte("{\"schema\":\"ui_evidence.v1\"}\n")
		if err := os.WriteFile(filepath.Join(root, "evidence.json"), evidenceBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		evidenceSHA = sha256Hex(evidenceBytes)
		source["evidence_schema"] = "ui_evidence.v1"
		source["evidence_sha256"] = evidenceSHA
		metadata["evidence_path"] = "evidence.json"
		metadata["evidence_sha256"] = evidenceSHA
	}
	layout := map[string]any{
		"schema": "ui_layout.v1",
		"source": source,
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
	layoutSHA := sha256Hex(layoutBytes)
	metadata["layout_sha256"] = layoutSHA
	metadata["layout_revision_sha256"] = "sha256:" + layoutSHA
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	revisionDir := filepath.Join(root, ".codeagent", "assets", "ui-layout", "layout_p1g", "revision-000001")
	if err := os.MkdirAll(revisionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(revisionDir, "layout.json"), layoutBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(revisionDir, "metadata.json"), metadataBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return testWorkspace{
		Root:          root,
		Layout:        LayoutRef{ArtifactID: "layout_p1g", Revision: 1},
		Identity:      LayoutIdentity{ArtifactID: "layout_p1g", Revision: 1, RevisionSHA256: "sha256:" + layoutSHA},
		Source:        SourceIdentity{AssetID: "asset_p1g_source", SHA256: sourceSHA},
		EvidenceSHA:   evidenceSHA,
		Targets:       sortedTargets,
		States:        append([]string(nil), states...),
		SourceSHA:     sourceSHA,
		FallbackBytes: readFixture(t, "fallback.png"),
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func testLineage(ws testWorkspace, target, state, matrixID string) *preview.Lineage {
	var evidence *string
	if ws.EvidenceSHA != "" {
		value := ws.EvidenceSHA
		evidence = &value
	}
	return &preview.Lineage{
		Schema:               preview.LineageSchema,
		LayoutArtifactID:     ws.Identity.ArtifactID,
		LayoutRevision:       ws.Identity.Revision,
		LayoutRevisionSHA256: ws.Identity.RevisionSHA256,
		SourceAssetID:        ws.Source.AssetID,
		SourceAssetSHA256:    ws.Source.SHA256,
		EvidenceSHA256:       evidence,
		Approval:             preview.LineageApproval{State: "not_required"},
		Target:               target,
		State:                state,
		TargetMatrix:         preview.LineageTargetMatrix{ID: matrixID, Targets: ws.Targets},
	}
}

type artifactOptions struct {
	ID        string
	Revision  int64
	Mode      preview.PreviewMode
	Lineage   *preview.Lineage
	Status    preview.Status
	Kind      preview.Kind
	ErrorCode preview.ErrorCode
	Message   string
	NoViewer  bool
}

func newTestStore(t *testing.T) (*preview.FSStore, *Builder) {
	t.Helper()
	store, err := preview.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store, NewBuilder(store)
}

// importArtifact writes one preview revision directly into the store, bypassing
// the producer so the matrix is tested against real persisted artifacts.
func importArtifact(t *testing.T, store *preview.FSStore, opts artifactOptions) preview.Artifact {
	t.Helper()
	revision := opts.Revision
	if revision == 0 {
		revision = 1
	}
	mode := opts.Mode
	if mode == "" {
		mode = preview.ModeAdaptation
	}
	status := opts.Status
	if status == "" {
		status = preview.StatusReady
	}
	kind := opts.Kind
	if kind == "" {
		kind = preview.KindInteractiveHTML
	}
	pngBytes := readFixture(t, "fallback.png")
	config, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	const assetID = "asset_p1g_fallback"
	asset := preview.Asset{
		AssetID:               assetID,
		Role:                  preview.RoleFallback,
		MIMEType:              "image/png",
		SizeBytes:             int64(len(pngBytes)),
		SHA256:                preview.DigestBytes(pngBytes),
		ContentURL:            preview.AssetURL(testSession, opts.ID, revision, assetID),
		Width:                 config.Width,
		Height:                config.Height,
		Alt:                   "Static evidence",
		WorkspaceRelativePath: "fallback.png",
	}
	viewerBytes := []byte("<html><body><div id=\"p1g\">preview</div></body></html>")
	metadata := map[string]any{preview.MetadataPreviewMode: string(mode)}
	if opts.Lineage != nil {
		metadata[preview.MetadataLineage] = opts.Lineage
	}
	artifact := preview.Artifact{
		SchemaVersion: preview.SchemaVersion,
		ArtifactID:    opts.ID,
		Revision:      revision,
		Status:        status,
		Kind:          kind,
		Title:         "P1-G matrix preview",
		Capability:    "ui-adaptation",
		ProducerID:    "ui_preview",
		SessionID:     testSession,
		TurnID:        "turn_p1g",
		CallID:        "call_p1g",
		CreatedAt:     testTime,
		UpdatedAt:     testTime,
		Presentation:  preview.DefaultPresentation(),
		Assets:        []preview.Asset{asset},
		Fallback:      &preview.Fallback{AssetID: assetID, Text: "Static evidence"},
		Security:      preview.DefaultSecurity(),
		Metadata:      metadata,
	}
	switch status {
	case preview.StatusFailed, preview.StatusUnavailable:
		artifact.Viewer = nil
		artifact.Error = &preview.Error{Code: opts.ErrorCode, Message: opts.Message, Retryable: true}
		if artifact.Error.Code == "" {
			artifact.Error.Code = preview.CodeRenderFailed
		}
		if artifact.Error.Message == "" {
			artifact.Error.Message = "Preview rendering failed."
		}
	case preview.StatusPending:
		artifact.Viewer = nil
	default:
		if !opts.NoViewer {
			artifact.Viewer = &preview.Viewer{
				URL:       preview.ViewerURL(testSession, opts.ID, revision),
				MIMEType:  preview.ViewerMIMEType,
				SizeBytes: int64(len(viewerBytes)),
				SHA256:    preview.DigestBytes(viewerBytes),
			}
		}
	}
	digest, err := preview.RevisionSHA256(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifact.RevisionSHA256 = digest
	if err := artifact.Validate(); err != nil {
		encoded, _ := json.MarshalIndent(artifact, "", "  ")
		t.Fatalf("fixture artifact is invalid: %v\n%s", err, encoded)
	}
	var viewer []byte
	if artifact.Viewer != nil {
		viewer = viewerBytes
	}
	assets := map[string][]byte{assetID: pngBytes}
	if err := store.ImportArtifact(context.Background(), preview.Namespace{SessionID: testSession}, artifact, viewer, assets); err != nil {
		t.Fatalf("import artifact: %v", err)
	}
	return artifact
}

func buildRequest(ws testWorkspace, matrixID string, cells []CellRef) Request {
	return Request{
		SessionID:     testSession,
		MatrixID:      matrixID,
		WorkspaceRoot: ws.Root,
		Layout:        ws.Layout,
		Cells:         cells,
	}
}

func entryAt(t *testing.T, m Matrix, target, state string) Entry {
	t.Helper()
	for _, entry := range m.Entries {
		if entry.Target == target && entry.State == state {
			return entry
		}
	}
	t.Fatalf("matrix has no entry for %s/%s", target, state)
	return Entry{}
}

func cloneMatrix(t *testing.T, m Matrix) Matrix {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out Matrix
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
