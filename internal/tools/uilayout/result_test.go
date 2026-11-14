package uilayout

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/uicontract"
	"code-agent/schemas"
)

func TestBuildResultMatchesGolden(t *testing.T) {
	output := buildResult(resultInputs{
		LayoutArtifactID:     "layout_fixture_commerce",
		LayoutRevision:       3,
		LayoutRevisionSHA256: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		SourceAssetID:        "asset_fixture_source",
		SourceAssetSHA256:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Canvas:               layoutCanvas{Width: 1290, Height: 2796},
		NodeCount:            3,
		AssumptionCount:      0,
		Targets:              []string{"iphone_portrait", "duo_folded"},
		Created:              true,
		OverlayAsset: makeResultAsset(
			"asset_turn_fixture_call_fixture_000_overlay",
			"layout_overlay", "image",
			".codeagent/assets/ui-layout/layout_fixture_commerce/revision-000003/overlay.png",
			"image/png",
			"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			1024, 2048,
		),
		LayoutJSONAsset: makeResultAsset(
			"asset_turn_fixture_call_fixture_001_layout",
			"layout_json", "file",
			".codeagent/assets/ui-layout/layout_fixture_commerce/revision-000003/layout.json",
			"application/json",
			"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			0, 0,
		),
	})
	got, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ui-layout-result", "v1", "golden", "basic.json"))
	if err != nil {
		t.Fatal(err)
	}
	var gotCompact, wantCompact bytes.Buffer
	if err := json.Compact(&gotCompact, got); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&wantCompact, want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCompact.Bytes(), wantCompact.Bytes()) {
		t.Fatalf("result golden mismatch\n got: %s\nwant: %s", gotCompact.Bytes(), wantCompact.Bytes())
	}
	if output.Targets[0] != "duo_folded" || output.Targets[1] != "iphone_portrait" {
		t.Fatalf("targets were not sorted: %v", output.Targets)
	}
	if output.EvidenceSHA256 != nil {
		t.Fatalf("missing evidence must serialize as null, got %q", *output.EvidenceSHA256)
	}
}

func TestExecuteResultMatchesSchemaMetadataAndAssets(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	tool := NewTool()

	first := execute(t, tool, root, args)
	if first.Failure != nil {
		t.Fatalf("execute failed: %+v", first.Failure)
	}
	if err := uicontract.ValidateAgainstSchema(schemas.UILayoutResultV1, first.Output); err != nil {
		t.Fatalf("result schema: %v", err)
	}
	if err := uicontract.ValidateUILayoutResult(first.Output); err != nil {
		t.Fatalf("result semantics: %v", err)
	}
	decoded := decodeResult(t, first)

	if len(first.Assets) != 2 {
		t.Fatalf("assets = %+v", first.Assets)
	}
	assetByID := map[string]int{}
	for i, ref := range first.Assets {
		assetByID[ref.ID] = i
	}
	overlayIndex, ok := assetByID[decoded.OverlayAsset.AssetID]
	if !ok {
		t.Fatal("overlay asset id is missing from the side-channel")
	}
	layoutIndex, ok := assetByID[decoded.LayoutJSONAsset.AssetID]
	if !ok {
		t.Fatal("layout JSON asset id is missing from the side-channel")
	}

	overlayRef := first.Assets[overlayIndex]
	layoutRef := first.Assets[layoutIndex]
	if overlayRef.WorkspaceRelativePath != decoded.OverlayAsset.RelativePath ||
		layoutRef.WorkspaceRelativePath != decoded.LayoutJSONAsset.RelativePath {
		t.Fatalf("asset paths do not match the result: %+v vs %+v", first.Assets, decoded)
	}

	layoutBytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(decoded.LayoutJSONAsset.RelativePath)))
	if err != nil {
		t.Fatalf("read layout asset: %v", err)
	}
	if got := digestBytes(layoutBytes); got != decoded.LayoutJSONAsset.SHA256 {
		t.Fatalf("layout asset digest = %s, want %s", decoded.LayoutJSONAsset.SHA256, got)
	}
	var parseable map[string]any
	if err := json.Unmarshal(layoutBytes, &parseable); err != nil {
		t.Fatalf("layout asset is not parseable JSON: %v", err)
	}

	overlayBytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(decoded.OverlayAsset.RelativePath)))
	if err != nil {
		t.Fatalf("read overlay asset: %v", err)
	}
	if got := digestBytes(overlayBytes); got != decoded.OverlayAsset.SHA256 {
		t.Fatalf("overlay asset digest = %s, want %s", decoded.OverlayAsset.SHA256, got)
	}

	metadataBytes, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Join(root, filepath.FromSlash(decoded.LayoutJSONAsset.RelativePath))), "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var metadata revisionMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if decoded.LayoutArtifactID != metadata.LayoutArtifactID ||
		decoded.LayoutRevision != metadata.LayoutRevision ||
		decoded.LayoutRevisionSHA256 != metadata.LayoutRevisionSHA256 ||
		decoded.SourceAssetID != metadata.SourceAssetID ||
		decoded.SourceAssetSHA256 != metadata.SourceAssetSHA256 ||
		decoded.Canvas != metadata.Canvas ||
		decoded.NodeCount != metadata.NodeCount ||
		decoded.AssumptionCount != metadata.AssumptionCount {
		t.Fatalf("result identity does not match metadata: result=%+v metadata=%+v", decoded, metadata)
	}
	if decoded.LayoutJSONAsset.RelativePath != metadata.LayoutPath ||
		decoded.LayoutJSONAsset.SHA256 != metadata.LayoutSHA256 {
		t.Fatalf("layout asset does not match metadata: %+v vs %+v", decoded.LayoutJSONAsset, metadata)
	}
	if decoded.OverlayAsset.RelativePath != metadata.OverlayPath ||
		decoded.OverlayAsset.Width != metadata.OverlayWidth ||
		decoded.OverlayAsset.Height != metadata.OverlayHeight ||
		decoded.OverlayAsset.SHA256 != metadata.OverlaySHA256 {
		t.Fatalf("overlay asset does not match metadata: %+v vs %+v", decoded.OverlayAsset, metadata)
	}
	if decoded.EvidenceSHA256 == nil || *decoded.EvidenceSHA256 != metadata.EvidenceSHA256 {
		t.Fatalf("evidence digest does not match metadata: %v vs %q", decoded.EvidenceSHA256, metadata.EvidenceSHA256)
	}

	second := execute(t, tool, root, args)
	if second.Failure != nil {
		t.Fatalf("second execute failed: %+v", second.Failure)
	}
	secondDecoded := decodeResult(t, second)
	if secondDecoded.Created {
		t.Fatal("identical call created a new revision")
	}
	secondDecoded.Created = true
	if !resultEqual(decoded, secondDecoded) {
		t.Fatalf("result is not stable across repeated calls:\nfirst:  %+v\nsecond: %+v", decoded, secondDecoded)
	}
}

func resultEqual(a, b result) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}
