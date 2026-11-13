package uilayout

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/tools"
)

func TestCanonicalLayoutMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ui-layout", "v1", "valid", "basic.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ui-layout", "v1", "golden", "basic.canonical.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical layout does not match golden\n got: %s\nwant: %s", got, want)
	}
	again, err := canonicalJSON(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, want) {
		t.Fatal("canonical layout is not stable across a second pass")
	}
}

func TestToolContract(t *testing.T) {
	tool := NewTool()
	if tool.Name() != "ui_layout" {
		t.Fatalf("name = %q", tool.Name())
	}
	schema := string(tool.InputSchema())
	for _, want := range []string{
		`"additionalProperties":false`,
		`"required":["source_path","layout"]`,
		`"source_path"`,
		`"evidence_path"`,
		`"artifact_id"`,
	} {
		if !strings.Contains(schema, want) {
			t.Fatalf("input schema missing %s: %s", want, schema)
		}
	}
	binding := tool.ToolBinding()
	if binding.Capability != "ui-adaptation" || binding.Executor != tools.ExecutorServer || binding.ModelVisibility != tools.VisibilityDirect {
		t.Fatalf("binding = %+v", binding)
	}
}

func TestRenderOverlayDownscalesLargeSources(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 3000, 100))
	root := layoutNode{ID: "n_root"}
	data, width, height, err := renderOverlay(source, root)
	if err != nil {
		t.Fatalf("render overlay: %v", err)
	}
	if width != maxOverlayDimension || height >= 100 || len(data) == 0 {
		t.Fatalf("overlay = %dx%d (%d bytes)", width, height, len(data))
	}
}

func TestExecutePersistsRevisionAndOverlay(t *testing.T) {
	root, sha, sourceBytes := testWorkspace(t)
	args := validArgs(sha)
	tool := NewTool()

	first := execute(t, tool, root, args)
	if first.Failure != nil {
		t.Fatalf("first execute failed: %+v", first.Failure)
	}
	firstResult := decodeResult(t, first)
	if !firstResult.Created || firstResult.LayoutRevision != 1 {
		t.Fatalf("first revision = %+v, want created revision 1", firstResult)
	}
	if firstResult.NodeCount != 3 || firstResult.AssumptionCount != 0 {
		t.Fatalf("node/assumption counts = %d/%d", firstResult.NodeCount, firstResult.AssumptionCount)
	}
	if firstResult.SourceAssetSHA256 != digestBytes(sourceBytes) || firstResult.SourceAssetSHA256 != sha {
		t.Fatalf("source sha = %s", firstResult.SourceAssetSHA256)
	}
	if firstResult.OverlayWidth != 200 || firstResult.OverlayHeight != 400 {
		t.Fatalf("overlay size = %dx%d", firstResult.OverlayWidth, firstResult.OverlayHeight)
	}
	if len(firstResult.Targets) != 2 || firstResult.Targets[0] != "duo_folded" || firstResult.Targets[1] != "iphone_portrait" {
		t.Fatalf("targets = %v", firstResult.Targets)
	}
	if !strings.HasPrefix(firstResult.LayoutRevisionSHA256, "sha256:") || len(firstResult.LayoutRevisionSHA256) != len("sha256:")+64 {
		t.Fatalf("revision sha = %q", firstResult.LayoutRevisionSHA256)
	}

	layoutPath := filepath.Join(root, filepath.FromSlash(firstResult.LayoutPath))
	layoutBytes, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatalf("read persisted layout: %v", err)
	}
	if got := "sha256:" + digestBytes(layoutBytes); got != firstResult.LayoutRevisionSHA256 {
		t.Fatalf("stored layout digest = %s, want %s", got, firstResult.LayoutRevisionSHA256)
	}
	var persisted layoutDocument
	if err := json.Unmarshal(layoutBytes, &persisted); err != nil {
		t.Fatalf("decode persisted layout: %v", err)
	}
	if persisted.Root.ID != "n_root" {
		t.Fatalf("persisted root = %q", persisted.Root.ID)
	}

	metadataBytes, err := os.ReadFile(filepath.Join(filepath.Dir(layoutPath), "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var metadata revisionMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata.LayoutRevision != 1 || metadata.SourceAssetSHA256 != sha || metadata.NodeCount != 3 {
		t.Fatalf("metadata = %+v", metadata)
	}

	overlayPath := filepath.Join(root, filepath.FromSlash(firstResult.OverlayPath))
	overlayBytes, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	overlay, err := png.Decode(bytes.NewReader(overlayBytes))
	if err != nil {
		t.Fatalf("decode overlay: %v", err)
	}
	if overlay.Bounds().Dx() != 200 || overlay.Bounds().Dy() != 400 {
		t.Fatalf("overlay bounds = %v", overlay.Bounds())
	}
	if len(first.Assets) != 1 || first.Assets[0].MIMEType != "image/png" {
		t.Fatalf("assets = %+v", first.Assets)
	}
	if first.Assets[0].Metadata["asset_role"] != "layout_overlay" {
		t.Fatalf("overlay asset metadata = %+v", first.Assets[0].Metadata)
	}

	if got := pixelAt(overlay, 20, 40); got != (color.NRGBA{34, 153, 84, 255}) {
		t.Fatalf("observed node border = %v, want green", got)
	}
	if got := pixelAt(overlay, 50, 200); got != (color.NRGBA{217, 139, 24, 255}) {
		t.Fatalf("inferred node border = %v, want amber", got)
	}

	second := execute(t, tool, root, args)
	if second.Failure != nil {
		t.Fatalf("second execute failed: %+v", second.Failure)
	}
	secondResult := decodeResult(t, second)
	if secondResult.Created || secondResult.LayoutRevision != 1 {
		t.Fatalf("identical layout revision = %+v, want reused revision 1", secondResult)
	}
	if secondResult.OverlayPath != firstResult.OverlayPath {
		t.Fatalf("reused overlay path = %q", secondResult.OverlayPath)
	}

	changed := cloneArgs(t, args)
	layout := changed["layout"].(map[string]any)
	layout["assumptions"] = []any{map[string]any{
		"statement":    "The action bar stays sticky.",
		"confidence":   0.6,
		"evidence":     "inferred",
		"evidence_ids": []any{"el_title"},
	}}
	third := execute(t, tool, root, changed)
	if third.Failure != nil {
		t.Fatalf("third execute failed: %+v", third.Failure)
	}
	thirdResult := decodeResult(t, third)
	if !thirdResult.Created || thirdResult.LayoutRevision != 2 {
		t.Fatalf("changed layout revision = %+v, want created revision 2", thirdResult)
	}
	if thirdResult.AssumptionCount != 1 {
		t.Fatalf("assumption count = %d", thirdResult.AssumptionCount)
	}
}

func TestExecuteValidatesEvidenceSHA256(t *testing.T) {
	t.Run("matching digest", func(t *testing.T) {
		root, sha, _ := testWorkspace(t)
		args := validArgs(sha)
		evidenceBytes := mustJSON(t, args["evidence"])
		layout := args["layout"].(map[string]any)
		layout["source"].(map[string]any)["evidence_sha256"] = digestBytes(evidenceBytes)

		result := execute(t, NewTool(), root, args)
		if result.Failure != nil {
			t.Fatalf("execute failed: %+v", result.Failure)
		}
		decoded := decodeResult(t, result)
		if decoded.EvidenceSHA256 != digestBytes(evidenceBytes) {
			t.Fatalf("evidence sha = %q", decoded.EvidenceSHA256)
		}
	})

	t.Run("mismatched digest", func(t *testing.T) {
		root, sha, _ := testWorkspace(t)
		args := validArgs(sha)
		layout := args["layout"].(map[string]any)
		layout["source"].(map[string]any)["evidence_sha256"] = strings.Repeat("0", 64)

		result := execute(t, NewTool(), root, args)
		if result.Failure == nil || result.Failure.Kind != "evidence_mismatch" {
			t.Fatalf("failure = %+v", result.Failure)
		}
	})

	t.Run("declared digest without evidence", func(t *testing.T) {
		root, sha, _ := testWorkspace(t)
		args := validArgs(sha)
		delete(args, "evidence")
		layout := args["layout"].(map[string]any)
		layout["source"].(map[string]any)["evidence_sha256"] = strings.Repeat("a", 64)

		result := execute(t, NewTool(), root, args)
		if result.Failure == nil || result.Failure.Kind != "evidence_mismatch" {
			t.Fatalf("failure = %+v", result.Failure)
		}
	})

	t.Run("evidence path digest", func(t *testing.T) {
		root, sha, _ := testWorkspace(t)
		args := validArgs(sha)
		evidenceBytes := mustJSON(t, args["evidence"])
		delete(args, "evidence")
		if err := os.WriteFile(filepath.Join(root, "evidence.json"), evidenceBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		args["evidence_path"] = "evidence.json"
		layout := args["layout"].(map[string]any)
		layout["source"].(map[string]any)["evidence_sha256"] = digestBytes(evidenceBytes)

		result := execute(t, NewTool(), root, args)
		if result.Failure != nil {
			t.Fatalf("execute failed: %+v", result.Failure)
		}
	})
}

func TestRevisionReuseRejectsTamperedArtifacts(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(t *testing.T, revisionDir string)
	}{
		{"metadata symlink", func(t *testing.T, revisionDir string) {
			replaceWithSymlink(t, filepath.Join(revisionDir, "metadata.json"))
		}},
		{"layout symlink", func(t *testing.T, revisionDir string) {
			replaceWithSymlink(t, filepath.Join(revisionDir, "layout.json"))
		}},
		{"overlay symlink", func(t *testing.T, revisionDir string) {
			replaceWithSymlink(t, filepath.Join(revisionDir, "overlay.png"))
		}},
		{"metadata path tampering", func(t *testing.T, revisionDir string) {
			path := filepath.Join(revisionDir, "metadata.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var metadata revisionMetadata
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata.LayoutPath = "../../outside/layout.json"
			metadata.OverlayPath = "/tmp/outside-overlay.png"
			if err := os.WriteFile(path, mustJSON(t, metadata), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, sha, _ := testWorkspace(t)
			artifactID := "layout_" + sha[:16]
			tool := NewTool()
			first := execute(t, tool, root, validArgs(sha))
			if first.Failure != nil {
				t.Fatalf("first execute failed: %+v", first.Failure)
			}
			firstResult := decodeResult(t, first)
			if firstResult.LayoutRevision != 1 {
				t.Fatalf("first revision = %d", firstResult.LayoutRevision)
			}
			revisionDir := filepath.Join(root, ".codeagent", "assets", "ui-layout", artifactID, revisionName(1))
			tc.tamper(t, revisionDir)

			second := execute(t, tool, root, validArgs(sha))
			if second.Failure != nil {
				t.Fatalf("second execute failed: %+v", second.Failure)
			}
			secondResult := decodeResult(t, second)
			if !secondResult.Created || secondResult.LayoutRevision != 2 {
				t.Fatalf("tampered revision was reused: %+v", secondResult)
			}
			assertInsideWorkspace(t, root, secondResult.LayoutPath)
			assertInsideWorkspace(t, root, secondResult.OverlayPath)
			if !strings.Contains(secondResult.LayoutPath, revisionName(2)) ||
				!strings.Contains(secondResult.OverlayPath, revisionName(2)) {
				t.Fatalf("revision paths do not point at the new revision: %+v", secondResult)
			}
			if secondResult.LayoutPath == "../../outside/layout.json" || secondResult.OverlayPath == "/tmp/outside-overlay.png" {
				t.Fatalf("tampered metadata path was trusted: %+v", secondResult)
			}
		})
	}
}

func TestMatchesWorkspaceRelativePath(t *testing.T) {
	expected := ".codeagent/assets/ui-layout/layout_x/revision-000001/layout.json"
	for _, value := range []string{
		expected,
		"./.codeagent/assets/ui-layout/layout_x/revision-000001/layout.json",
	} {
		if !matchesWorkspaceRelativePath(value, expected) {
			t.Fatalf("%q should match %q", value, expected)
		}
	}
	for _, value := range []string{
		"",
		"../outside/layout.json",
		"../../outside/layout.json",
		"/tmp/layout.json",
		`..\outside\layout.json`,
		" .codeagent/assets/layout.json",
		".codeagent/assets/ui-layout/layout_y/revision-000001/layout.json",
	} {
		if matchesWorkspaceRelativePath(value, expected) {
			t.Fatalf("%q should not match %q", value, expected)
		}
	}
}

func replaceWithSymlink(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func assertInsideWorkspace(t *testing.T, root, workspaceRelative string) {
	t.Helper()
	if workspaceRelative == "" || filepath.IsAbs(workspaceRelative) {
		t.Fatalf("path %q is not workspace-relative", workspaceRelative)
	}
	abs := filepath.Join(root, filepath.FromSlash(workspaceRelative))
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("path %q escapes the workspace", workspaceRelative)
	}
}

func TestExecuteAcceptsLayoutWithoutEvidence(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	layout := args["layout"].(map[string]any)
	rootNode := layout["root"].(map[string]any)
	rootNode["evidence_ids"] = []any{}
	for _, child := range rootNode["children"].([]any) {
		child.(map[string]any)["evidence_ids"] = []any{}
	}
	delete(args, "evidence")

	result := execute(t, NewTool(), root, args)
	if result.Failure != nil {
		t.Fatalf("execute failed: %+v", result.Failure)
	}
	decoded := decodeResult(t, result)
	if decoded.EvidenceSHA256 != "" || decoded.EvidencePath != "" {
		t.Fatalf("unexpected evidence identity: %+v", decoded)
	}
}

func TestExecuteCustomArtifactID(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	args["artifact_id"] = "layout_custom_test"
	result := execute(t, NewTool(), root, args)
	if result.Failure != nil {
		t.Fatalf("execute failed: %+v", result.Failure)
	}
	decoded := decodeResult(t, result)
	if decoded.LayoutArtifactID != "layout_custom_test" {
		t.Fatalf("artifact id = %q", decoded.LayoutArtifactID)
	}
	if !strings.Contains(decoded.LayoutPath, "/layout_custom_test/") {
		t.Fatalf("layout path = %q", decoded.LayoutPath)
	}
}

func TestExecuteEvidencePath(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	evidence := args["evidence"]
	delete(args, "evidence")
	evidenceBytes := mustJSON(t, evidence)
	if err := os.WriteFile(filepath.Join(root, "evidence.json"), evidenceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	args["evidence_path"] = "evidence.json"
	args["layout"].(map[string]any)["source"].(map[string]any)["evidence_sha256"] = digestBytes(evidenceBytes)
	result := execute(t, NewTool(), root, args)
	if result.Failure != nil {
		t.Fatalf("execute failed: %+v", result.Failure)
	}
	decoded := decodeResult(t, result)
	if decoded.EvidencePath != "evidence.json" || decoded.EvidenceSHA256 != digestBytes(evidenceBytes) {
		t.Fatalf("evidence identity = %+v", decoded)
	}
}

func TestExecuteEvidencePathRequiresEvidenceSHA(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	evidenceBytes := mustJSON(t, args["evidence"])
	delete(args, "evidence")
	if err := os.WriteFile(filepath.Join(root, "evidence.json"), evidenceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	args["evidence_path"] = "evidence.json"

	result := execute(t, NewTool(), root, args)
	if result.Failure == nil || result.Failure.Kind != "evidence_mismatch" {
		t.Fatalf("failure = %+v", result.Failure)
	}
	if !strings.Contains(result.Failure.Message, "evidence_sha256") {
		t.Fatalf("failure message = %q", result.Failure.Message)
	}
}

func TestExecuteEvidencePathRejectsTamperedEvidence(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	evidenceBytes := mustJSON(t, args["evidence"])
	delete(args, "evidence")
	evidencePath := filepath.Join(root, "evidence.json")
	if err := os.WriteFile(evidencePath, evidenceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	args["evidence_path"] = "evidence.json"
	args["layout"].(map[string]any)["source"].(map[string]any)["evidence_sha256"] = digestBytes(evidenceBytes)

	var evidence map[string]any
	if err := json.Unmarshal(evidenceBytes, &evidence); err != nil {
		t.Fatal(err)
	}
	element := evidence["elements"].([]any)[0].(map[string]any)
	element["text"] = "Tampered"
	if err := os.WriteFile(evidencePath, mustJSON(t, evidence), 0o600); err != nil {
		t.Fatal(err)
	}

	result := execute(t, NewTool(), root, args)
	if result.Failure == nil || result.Failure.Kind != "evidence_mismatch" {
		t.Fatalf("failure = %+v", result.Failure)
	}
}

func TestExecuteEvidencePathRejectsSourceAssetIDMismatch(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	evidence := args["evidence"].(map[string]any)
	delete(args, "evidence")
	evidence["source"].(map[string]any)["asset_id"] = "asset_source_other"
	evidenceBytes := mustJSON(t, evidence)
	if err := os.WriteFile(filepath.Join(root, "evidence.json"), evidenceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	args["evidence_path"] = "evidence.json"
	args["layout"].(map[string]any)["source"].(map[string]any)["evidence_sha256"] = digestBytes(evidenceBytes)

	result := execute(t, NewTool(), root, args)
	if result.Failure == nil || result.Failure.Kind != "evidence_mismatch" {
		t.Fatalf("failure = %+v", result.Failure)
	}
	if !strings.Contains(result.Failure.Message, "asset_id") {
		t.Fatalf("failure message = %q", result.Failure.Message)
	}
}

func TestExecuteEvidencePathRejectsCanvasMismatch(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	evidence := args["evidence"].(map[string]any)
	delete(args, "evidence")
	evidence["source"].(map[string]any)["width"] = 201
	evidenceBytes := mustJSON(t, evidence)
	if err := os.WriteFile(filepath.Join(root, "evidence.json"), evidenceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	args["evidence_path"] = "evidence.json"
	args["layout"].(map[string]any)["source"].(map[string]any)["evidence_sha256"] = digestBytes(evidenceBytes)

	result := execute(t, NewTool(), root, args)
	if result.Failure == nil || result.Failure.Kind != "evidence_mismatch" {
		t.Fatalf("failure = %+v", result.Failure)
	}
}

func TestExecuteEvidencePathRejectsInvalidPaths(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	if err := os.Mkdir(filepath.Join(root, "evidence-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideEvidence := filepath.Join(outside, "evidence.json")
	if err := os.WriteFile(outsideEvidence, mustJSON(t, baseEvidence(sha)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideEvidence, filepath.Join(root, "evidence-link")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		kind string
	}{
		{"missing file", "missing.json", "not_found"},
		{"directory path", "evidence-dir", "invalid_arguments"},
		{"symlink escape", "evidence-link", "blocked"},
		{"absolute path", "/tmp/evidence.json", "blocked"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := validArgs(sha)
			delete(args, "evidence")
			args["evidence_path"] = tc.path
			result := execute(t, NewTool(), root, args)
			if result.Failure == nil || result.Failure.Kind != tc.kind {
				t.Fatalf("failure = %+v, want %q", result.Failure, tc.kind)
			}
		})
	}
}

func TestExecuteEvidencePathReuseIsDeterministic(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	args := validArgs(sha)
	evidenceBytes := mustJSON(t, args["evidence"])
	delete(args, "evidence")
	if err := os.WriteFile(filepath.Join(root, "evidence.json"), evidenceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	args["evidence_path"] = "evidence.json"
	args["layout"].(map[string]any)["source"].(map[string]any)["evidence_sha256"] = digestBytes(evidenceBytes)

	tool := NewTool()
	first := execute(t, tool, root, args)
	if first.Failure != nil {
		t.Fatalf("first execute failed: %+v", first.Failure)
	}
	firstResult := decodeResult(t, first)
	second := execute(t, tool, root, args)
	if second.Failure != nil {
		t.Fatalf("second execute failed: %+v", second.Failure)
	}
	secondResult := decodeResult(t, second)
	if secondResult.Created || secondResult.LayoutRevision != firstResult.LayoutRevision {
		t.Fatalf("second revision = %+v, want reused %d", secondResult, firstResult.LayoutRevision)
	}
	if secondResult.EvidenceSHA256 != firstResult.EvidenceSHA256 || secondResult.EvidencePath != firstResult.EvidencePath {
		t.Fatalf("evidence identity changed across repeats: %+v vs %+v", firstResult, secondResult)
	}
	if secondResult.LayoutPath != firstResult.LayoutPath || secondResult.OverlayPath != firstResult.OverlayPath {
		t.Fatalf("revision paths changed across repeats: %+v vs %+v", firstResult, secondResult)
	}
}

func TestExecuteRejectsInvalidInputs(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	tests := []struct {
		name string
		args map[string]any
		kind string
	}{
		{
			name: "unknown top-level input field",
			args: withExtra(validArgs(sha), "unexpected", true),
			kind: "invalid_arguments",
		},
		{
			name: "unknown layout field",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				layout["unexpected"] = true
			}),
			kind: "invalid_layout",
		},
		{
			name: "bbox outside normalized canvas",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				child := layout["root"].(map[string]any)["children"].([]any)[0].(map[string]any)
				child["bbox"] = map[string]any{"x": 0.9, "y": 0.1, "width": 0.5, "height": 0.04}
			}),
			kind: "invalid_layout",
		},
		{
			name: "zero-area bbox",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				rootNode := layout["root"].(map[string]any)
				rootNode["bbox"] = map[string]any{"x": 0, "y": 0, "width": 0, "height": 0}
			}),
			kind: "invalid_layout",
		},
		{
			name: "duplicate node id",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				child := layout["root"].(map[string]any)["children"].([]any)[0].(map[string]any)
				child["id"] = "n_root"
			}),
			kind: "invalid_layout",
		},
		{
			name: "unknown policy node",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				responsive := layout["responsive"].(map[string]any)
				responsive["node_policies"] = append(responsive["node_policies"].([]any),
					map[string]any{"node_id": "n_missing", "policy": "preserve"})
			}),
			kind: "invalid_layout",
		},
		{
			name: "duplicate invariant id",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				responsive := layout["responsive"].(map[string]any)
				responsive["invariants"] = append(responsive["invariants"].([]any),
					map[string]any{"id": "inv_default", "statement": "Duplicate."})
			}),
			kind: "invalid_layout",
		},
		{
			name: "unknown evidence id",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				child := layout["root"].(map[string]any)["children"].([]any)[0].(map[string]any)
				child["evidence_ids"] = []any{"el_missing"}
			}),
			kind: "invalid_layout",
		},
		{
			name: "evidence references without evidence document",
			args: func() map[string]any {
				args := validArgs(sha)
				delete(args, "evidence")
				return args
			}(),
			kind: "evidence_mismatch",
		},
		{
			name: "evidence source mismatch",
			args: mutateEvidence(validArgs(sha), func(evidence map[string]any) {
				evidence["source"].(map[string]any)["sha256"] = strings.Repeat("0", 64)
			}),
			kind: "evidence_mismatch",
		},
		{
			name: "evidence and evidence_path together",
			args: func() map[string]any {
				args := validArgs(sha)
				args["evidence_path"] = "evidence.json"
				return args
			}(),
			kind: "invalid_arguments",
		},
		{
			name: "source sha mismatch",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				layout["source"].(map[string]any)["source_asset_sha256"] = strings.Repeat("0", 64)
			}),
			kind: "source_mismatch",
		},
		{
			name: "canvas mismatch",
			args: mutateLayout(validArgs(sha), func(layout map[string]any) {
				layout["source"].(map[string]any)["canvas"] = map[string]any{"width": 201, "height": 400}
			}),
			kind: "source_mismatch",
		},
		{
			name: "source path traversal",
			args: withExtra(validArgs(sha), "source_path", "../outside.png"),
			kind: "blocked",
		},
		{
			name: "invalid artifact id",
			args: withExtra(validArgs(sha), "artifact_id", "not a layout id"),
			kind: "invalid_arguments",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := execute(t, NewTool(), root, tc.args)
			if result.Failure == nil {
				t.Fatalf("expected failure %q, got success: %s", tc.kind, result.Content)
			}
			if result.Failure.Kind != tc.kind {
				t.Fatalf("failure kind = %q (%s), want %q", result.Failure.Kind, result.Failure.Message, tc.kind)
			}
		})
	}
}

func TestExecuteRejectsSymlinkedAssetRoot(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	outside := t.TempDir()
	assetsDir := filepath.Join(root, ".codeagent", "assets")
	if err := os.MkdirAll(assetsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(assetsDir, "ui-layout")); err != nil {
		t.Fatal(err)
	}
	result := execute(t, NewTool(), root, validArgs(sha))
	if result.Failure == nil || result.Failure.Kind != "persist_failed" {
		t.Fatalf("failure = %+v", result.Failure)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("ui_layout wrote through a symlink: %v", entries)
	}
}

func TestExecuteRejectsUnsupportedSourceFormat(t *testing.T) {
	root, sha, _ := testWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "fake.png"), []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := validArgs(sha)
	args["source_path"] = "fake.png"
	result := execute(t, NewTool(), root, args)
	if result.Failure == nil || result.Failure.Kind != "unsupported_format" {
		t.Fatalf("failure = %+v", result.Failure)
	}
}

func testWorkspace(t *testing.T) (string, string, []byte) {
	t.Helper()
	root := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 200, 400))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(20, 40, 120, 60), image.NewUniform(color.RGBA{230, 230, 230, 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if err := os.WriteFile(filepath.Join(root, "source.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, digestBytes(data), data
}

func validArgs(sha string) map[string]any {
	return map[string]any{
		"source_path": "source.png",
		"layout":      baseLayout(sha),
		"evidence":    baseEvidence(sha),
	}
}

func baseLayout(sha string) map[string]any {
	return map[string]any{
		"schema": "ui_layout.v1",
		"source": map[string]any{
			"asset_id":            "asset_source_test",
			"source_asset_sha256": sha,
			"evidence_schema":     "ui_evidence.v1",
			"canvas":              map[string]any{"width": 200, "height": 400},
		},
		"screen": map[string]any{
			"type":             "fixture_detail",
			"primary_action":   "Continue",
			"content_priority": []any{"title", "action"},
			"states":           []any{"default"},
		},
		"root": map[string]any{
			"id":           "n_root",
			"role":         "screen",
			"bbox":         map[string]any{"x": 0.0, "y": 0.0, "width": 1.0, "height": 1.0},
			"layout":       map[string]any{"mode": "vertical_stack"},
			"evidence_ids": []any{},
			"children": []any{
				map[string]any{
					"id":           "n_title",
					"role":         "heading",
					"bbox":         map[string]any{"x": 0.1, "y": 0.1, "width": 0.5, "height": 0.05},
					"content":      map[string]any{"text": "Fixture"},
					"layout":       map[string]any{"mode": "absolute"},
					"evidence_ids": []any{"el_title"},
					"confidence":   0.95,
					"evidence":     "observed",
				},
				map[string]any{
					"id":           "n_action",
					"role":         "button",
					"bbox":         map[string]any{"x": 0.25, "y": 0.5, "width": 0.5, "height": 0.1},
					"content":      map[string]any{"control_label": "Continue"},
					"layout":       map[string]any{"mode": "absolute"},
					"behavior":     map[string]any{"sticky": true, "action": "continue"},
					"evidence_ids": []any{"el_title"},
					"confidence":   0.7,
					"evidence":     "inferred",
				},
			},
			"confidence": 0.9,
			"evidence":   "inferred",
		},
		"responsive": map[string]any{
			"invariants":    []any{map[string]any{"id": "inv_default", "statement": "The action remains discoverable."}},
			"targets":       map[string]any{"iphone_portrait": map[string]any{"columns": 1}, "duo_folded": map[string]any{"columns": 1}},
			"node_policies": []any{map[string]any{"node_id": "n_root", "policy": "reflow"}},
		},
		"assumptions": []any{},
		"confidence":  0.8,
	}
}

func baseEvidence(sha string) map[string]any {
	return map[string]any{
		"schema": "ui_evidence.v1",
		"source": map[string]any{
			"asset_id":    "asset_source_test",
			"source_kind": "static_image",
			"sha256":      sha,
			"mime_type":   "image/png",
			"width":       200,
			"height":      400,
		},
		"coordinate_space": "normalized_top_left",
		"producer":         map[string]any{"name": "test", "version": "1"},
		"elements": []any{
			map[string]any{
				"id":          "el_title",
				"kind":        "text",
				"bbox":        map[string]any{"x": 0.1, "y": 0.1, "width": 0.5, "height": 0.05},
				"text":        "Fixture",
				"confidence":  0.95,
				"evidence":    "observed",
				"parent_id":   nil,
				"interactive": false,
			},
		},
		"repeated_groups": []any{},
		"source_tree":     map[string]any{"kind": "none", "nodes": []any{}, "truncated": false},
		"crops":           []any{},
		"uncertainties":   []any{},
		"limits":          map[string]any{"element_count": 1, "elements_truncated": false, "tree_nodes_truncated": false},
	}
}

func execute(t *testing.T, tool *Tool, root string, args map[string]any) tools.ToolResult {
	t.Helper()
	raw := mustJSON(t, args)
	result, err := tool.Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_test",
		CallID:        "call_test",
	}, raw)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return result
}

func decodeResult(t *testing.T, toolResult tools.ToolResult) result {
	t.Helper()
	var decoded result
	if err := json.Unmarshal(toolResult.Output, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return decoded
}

func pixelAt(img image.Image, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

func withExtra(args map[string]any, key string, value any) map[string]any {
	out := cloneArgs(nil, args)
	out[key] = value
	return out
}

func mutateLayout(args map[string]any, mutate func(map[string]any)) map[string]any {
	out := cloneArgs(nil, args)
	mutate(out["layout"].(map[string]any))
	return out
}

func mutateEvidence(args map[string]any, mutate func(map[string]any)) map[string]any {
	out := cloneArgs(nil, args)
	mutate(out["evidence"].(map[string]any))
	return out
}

func cloneArgs(t *testing.T, args map[string]any) map[string]any {
	if t != nil {
		t.Helper()
	}
	data, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
