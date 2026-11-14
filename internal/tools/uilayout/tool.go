// Package uilayout implements the built-in ui_layout tool. The main model
// authors the layout document; this package only validates it, binds it to a
// workspace source image, persists a traceable revision, and renders an
// annotated overlay. It never infers or fills in layout content.
package uilayout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"code-agent/internal/assetref"
	"code-agent/internal/tools"
	"code-agent/internal/uicontract"
	"code-agent/internal/workspace"
	"code-agent/schemas"
)

const (
	DefaultMaxImageBytes  int64 = 32 << 20
	DefaultMaxImagePixels int64 = 40_000_000
	DefaultMaxJSONBytes         = 4 << 20
)

// Tool validates and persists model-authored ui_layout.v1 documents.
type Tool struct {
	MaxImageBytes  int64
	MaxImagePixels int64
	MaxJSONBytes   int

	mu sync.Mutex
}

func NewTool() *Tool {
	return &Tool{
		MaxImageBytes:  DefaultMaxImageBytes,
		MaxImagePixels: DefaultMaxImagePixels,
		MaxJSONBytes:   DefaultMaxJSONBytes,
	}
}

func (*Tool) Name() string { return "ui_layout" }

func (*Tool) Description() string {
	return "Validate and persist a model-authored ui_layout.v1 document against a workspace source image and optional ui_evidence.v1. " +
		"Checks the frozen schema, source SHA-256, canvas dimensions, node IDs, normalized bboxes, responsive invariants, and evidence references. " +
		"When evidence_path is supplied the layout must declare source.evidence_schema=ui_evidence.v1 and a source.evidence_sha256 matching the file bytes. " +
		"Returns the stored layout revision and an annotated overlay image. This tool does not generate or infer layout content."
}

func (*Tool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"source_path": {
			Type:        "string",
			Description: "Workspace-relative PNG or JPEG source image path.",
		},
		"layout": {
			Type:        "object",
			Description: "Model-authored ui_layout.v1 JSON object. Stored unchanged in meaning; unknown fields are rejected.",
		},
		"evidence_path": {
			Type:        "string",
			Description: "Optional workspace-relative ui_evidence.v1 JSON file. The layout must declare source.evidence_schema=ui_evidence.v1 and source.evidence_sha256 matching this file's bytes; source.asset_id must equal the evidence source asset_id.",
		},
		"evidence": {
			Type:        "object",
			Description: "Optional inline ui_evidence.v1 JSON object. Mutually exclusive with evidence_path.",
		},
		"artifact_id": {
			Type:        "string",
			Description: "Optional stable layout artifact ID. Defaults to a digest of the source image.",
		},
	}, "source_path", "layout").Closed().JSON()
}

func (*Tool) ToolBinding() tools.Binding {
	return tools.Binding{
		Name:            "ui_layout",
		ModelVisibility: tools.VisibilityDirect,
		Executor:        tools.ExecutorServer,
		Capability:      "ui-adaptation",
	}
}

type input struct {
	SourcePath   string          `json:"source_path"`
	Layout       json.RawMessage `json:"layout"`
	Evidence     json.RawMessage `json:"evidence"`
	EvidencePath string          `json:"evidence_path"`
	ArtifactID   string          `json:"artifact_id"`
}

type evidenceSource struct {
	AssetID string `json:"asset_id"`
	SHA256  string `json:"sha256"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

type evidenceDocument struct {
	Source evidenceSource `json:"source"`
}

type codedError struct {
	kind    string
	message string
}

func (e *codedError) Error() string { return e.message }

func coded(kind, message string) error { return &codedError{kind: kind, message: message} }

func (t *Tool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.ToolResult{}, err
	}
	maxBytes := t.MaxImageBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxImageBytes
	}
	maxPixels := t.MaxImagePixels
	if maxPixels <= 0 {
		maxPixels = DefaultMaxImagePixels
	}
	maxJSON := t.MaxJSONBytes
	if maxJSON <= 0 {
		maxJSON = DefaultMaxJSONBytes
	}

	var in input
	if err := decodeStrict(raw, &in); err != nil {
		return tools.FailureResult("invalid_arguments", "ui_layout input is invalid: "+err.Error()), nil
	}
	if strings.TrimSpace(in.SourcePath) == "" {
		return tools.FailureResult("invalid_arguments", "ui_layout requires source_path"), nil
	}
	if len(in.Layout) == 0 || string(in.Layout) == "null" {
		return tools.FailureResult("invalid_arguments", "ui_layout requires layout"), nil
	}
	if len(in.Layout) > maxJSON {
		return tools.FailureResult("invalid_arguments", "ui_layout layout exceeds the 4 MiB document budget."), nil
	}
	if len(in.Evidence) > 0 && string(in.Evidence) != "null" && strings.TrimSpace(in.EvidencePath) != "" {
		return tools.FailureResult("invalid_arguments", "ui_layout accepts evidence or evidence_path, not both"), nil
	}
	if len(in.Evidence) > maxJSON {
		return tools.FailureResult("invalid_arguments", "ui_layout evidence exceeds the 4 MiB document budget."), nil
	}
	if strings.TrimSpace(ec.WorkspaceRoot) == "" {
		return tools.FailureResult("unavailable", "ui_layout requires a workspace root"), nil
	}

	sourceAbs, sourceRel, err := resolveWorkspaceFile(ec.WorkspaceRoot, in.SourcePath, "source_path")
	if err != nil {
		return failureFromError(err), nil
	}
	sourceData, err := readBoundedFile(sourceAbs, maxBytes)
	if err != nil {
		return failureFromError(err), nil
	}
	mimeType := sniffMIMEType(sourceData)
	if mimeType == "" {
		return tools.FailureResult("unsupported_format", "ui_layout supports PNG and JPEG source images for overlay rendering."), nil
	}
	sourceImage, err := decodeImage(sourceData, mimeType)
	if err != nil {
		return tools.FailureResult("invalid_image", "ui_layout could not decode the source image: "+err.Error()), nil
	}
	bounds := sourceImage.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if int64(width)*int64(height) > maxPixels {
		return tools.FailureResult("image_too_large", fmt.Sprintf("ui_layout source image has %d pixels; maximum is %d.", int64(width)*int64(height), maxPixels)), nil
	}
	sourceSHA := digestBytes(sourceData)

	if err := uicontract.ValidateAgainstSchema(schemas.UILayoutV1, in.Layout); err != nil {
		return tools.FailureResult("invalid_layout", "ui_layout document does not match ui_layout.v1: "+err.Error()), nil
	}
	document, err := decodeLayout(in.Layout)
	if err != nil {
		return tools.FailureResult("invalid_layout", "ui_layout document could not be decoded: "+err.Error()), nil
	}
	if document.Source.SourceAssetSHA256 != sourceSHA {
		return tools.FailureResult("source_mismatch", "ui_layout source_asset_sha256 does not match the source image bytes."), nil
	}
	if document.Source.Canvas.Width != width || document.Source.Canvas.Height != height {
		return tools.FailureResult("source_mismatch", fmt.Sprintf(
			"ui_layout canvas %dx%d does not match the source image %dx%d.",
			document.Source.Canvas.Width, document.Source.Canvas.Height, width, height,
		)), nil
	}

	var evidenceData []byte
	evidenceRel := ""
	evidenceSHA := ""
	evidenceFromPath := strings.TrimSpace(in.EvidencePath) != ""
	if evidenceFromPath {
		evidenceAbs, rel, err := resolveWorkspaceFile(ec.WorkspaceRoot, in.EvidencePath, "evidence_path")
		if err != nil {
			return failureFromError(err), nil
		}
		evidenceData, err = readBoundedFile(evidenceAbs, int64(maxJSON))
		if err != nil {
			return failureFromError(err), nil
		}
		evidenceRel = rel
	} else if len(in.Evidence) > 0 && string(in.Evidence) != "null" {
		evidenceData = in.Evidence
	}
	if len(evidenceData) > 0 {
		if err := uicontract.ValidateAgainstSchema(schemas.UIEvidenceV1, evidenceData); err != nil {
			return tools.FailureResult("evidence_mismatch", "ui_layout evidence does not match ui_evidence.v1: "+err.Error()), nil
		}
		if err := uicontract.ValidateUIEvidence(evidenceData); err != nil {
			return tools.FailureResult("evidence_mismatch", "ui_layout evidence is invalid: "+err.Error()), nil
		}
		var evidence evidenceDocument
		if err := json.Unmarshal(evidenceData, &evidence); err != nil {
			return tools.FailureResult("evidence_mismatch", "ui_layout evidence could not be decoded: "+err.Error()), nil
		}
		if evidence.Source.SHA256 != sourceSHA {
			return tools.FailureResult("evidence_mismatch", "ui_layout evidence source sha256 does not match the source image."), nil
		}
		if evidence.Source.Width != width || evidence.Source.Height != height {
			return tools.FailureResult("evidence_mismatch", fmt.Sprintf(
				"ui_layout evidence canvas %dx%d does not match the source image %dx%d.",
				evidence.Source.Width, evidence.Source.Height, width, height,
			)), nil
		}
		if document.Source.AssetID != evidence.Source.AssetID {
			return tools.FailureResult("evidence_mismatch", "ui_layout source asset_id does not match the evidence source asset."), nil
		}
		if document.Source.SourceAssetSHA256 != evidence.Source.SHA256 {
			return tools.FailureResult("evidence_mismatch", "ui_layout source_asset_sha256 does not match the evidence source sha256."), nil
		}
		evidenceSHA = digestBytes(evidenceData)
		if evidenceFromPath {
			// A persisted evidence file carries a known content digest. Bind the
			// layout to it explicitly so a swapped or edited file cannot pass as
			// the evidence the layout was authored against.
			if document.Source.EvidenceSchema != "ui_evidence.v1" {
				return tools.FailureResult("evidence_mismatch", "ui_layout source.evidence_schema must be ui_evidence.v1 when evidence_path is supplied."), nil
			}
			if document.Source.EvidenceSHA256 == "" {
				return tools.FailureResult("evidence_mismatch", "ui_layout source.evidence_sha256 is required when evidence_path is supplied."), nil
			}
			if document.Source.EvidenceSHA256 != evidenceSHA {
				return tools.FailureResult("evidence_mismatch", "ui_layout evidence_sha256 does not match the evidence file bytes."), nil
			}
		} else if document.Source.EvidenceSHA256 != "" && document.Source.EvidenceSHA256 != evidenceSHA {
			return tools.FailureResult("evidence_mismatch", "ui_layout evidence_sha256 does not match the supplied ui_evidence.v1 bytes."), nil
		}
	} else if layoutUsesEvidence(document) || document.Source.EvidenceSHA256 != "" {
		return tools.FailureResult("evidence_mismatch", "ui_layout references ui_evidence.v1 but no evidence document was supplied."), nil
	}

	if err := uicontract.ValidateUILayout(in.Layout, evidenceData); err != nil {
		return tools.FailureResult("invalid_layout", "ui_layout semantics are invalid: "+err.Error()), nil
	}

	artifactID := strings.TrimSpace(in.ArtifactID)
	if artifactID == "" {
		artifactID = "layout_" + sourceSHA[:16]
	}
	if !artifactIDPattern.MatchString(artifactID) {
		return tools.FailureResult("invalid_arguments", "ui_layout artifact_id must match layout_[A-Za-z0-9._-]{1,180}."), nil
	}

	canonicalLayout, err := canonicalJSON(in.Layout)
	if err != nil {
		return tools.FailureResult("invalid_layout", "ui_layout document is not canonicalizable JSON: "+err.Error()), nil
	}
	revisionSHA := "sha256:" + digestBytes(canonicalLayout)
	overlayData, overlayWidth, overlayHeight, err := renderOverlay(sourceImage, document.Root)
	if err != nil {
		return tools.FailureResult("runtime", "ui_layout could not render the overlay: "+err.Error()), nil
	}

	sourceAssetID := document.Source.AssetID
	overlaySHA := digestBytes(overlayData)
	nodeCount := countLayoutNodes(document.Root)
	assumptionCount := len(document.Assumptions)
	meta := revisionMetadata{
		Kind:                 "ui_layout_revision",
		LayoutArtifactID:     artifactID,
		LayoutRevisionSHA256: revisionSHA,
		LayoutSHA256:         digestBytes(canonicalLayout),
		OverlayWidth:         overlayWidth,
		OverlayHeight:        overlayHeight,
		OverlaySHA256:        overlaySHA,
		SourcePath:           sourceRel,
		SourceAssetID:        sourceAssetID,
		SourceAssetSHA256:    sourceSHA,
		SourceMIMEType:       mimeType,
		Canvas:               document.Source.Canvas,
		NodeCount:            nodeCount,
		AssumptionCount:      assumptionCount,
	}
	if evidenceRel != "" {
		meta.EvidencePath = evidenceRel
	}
	meta.EvidenceSHA256 = evidenceSHA

	t.mu.Lock()
	persisted, err := persistRevision(ec.WorkspaceRoot, artifactID, canonicalLayout, overlayData, meta)
	t.mu.Unlock()
	if err != nil {
		return tools.FailureResult("persist_failed", "ui_layout could not persist the revision: "+err.Error()), nil
	}

	targets := document.targets()
	sort.Strings(targets)

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}
	workspaceID := assets.WorkspaceID(rootAbs)
	overlayAbs := filepath.Join(rootAbs, filepath.FromSlash(persisted.OverlayPath))
	layoutAbs := filepath.Join(rootAbs, filepath.FromSlash(persisted.LayoutPath))
	overlayAssetID := assets.StableID(ec.TurnID, ec.CallID, 0, revisionSHA, "ui_layout_overlay")
	layoutAssetID := assets.StableID(ec.TurnID, ec.CallID, 1, revisionSHA, "ui_layout_layout_json")

	output := buildResult(resultInputs{
		LayoutArtifactID:     artifactID,
		LayoutRevision:       persisted.Revision,
		LayoutRevisionSHA256: revisionSHA,
		SourceAssetID:        sourceAssetID,
		SourceAssetSHA256:    sourceSHA,
		EvidenceSHA256:       evidenceSHA,
		Canvas:               document.Source.Canvas,
		NodeCount:            nodeCount,
		AssumptionCount:      assumptionCount,
		Targets:              targets,
		Created:              persisted.Created,
		OverlayAsset: makeResultAsset(
			overlayAssetID, "layout_overlay", "image", persisted.OverlayPath, "image/png", overlaySHA, overlayWidth, overlayHeight,
		),
		LayoutJSONAsset: makeResultAsset(
			layoutAssetID, "layout_json", "file", persisted.LayoutPath, "application/json", meta.LayoutSHA256, 0, 0,
		),
	})
	outputJSON, err := output.marshal()
	if err != nil {
		return tools.ToolResult{}, err
	}

	overlayRef := assets.Ref{
		ID:                    overlayAssetID,
		Kind:                  "image",
		URI:                   assets.WorkspaceURI(workspaceID, persisted.OverlayPath, nil),
		DisplayName:           filepath.Base(persisted.OverlayPath),
		WorkspaceID:           workspaceID,
		WorkspaceRelativePath: persisted.OverlayPath,
		AbsolutePath:          overlayAbs,
		MIMEType:              "image/png",
		Metadata: map[string]any{
			"asset_role":             "layout_overlay",
			"layout_artifact_id":     artifactID,
			"layout_revision":        persisted.Revision,
			"layout_revision_sha256": revisionSHA,
			"source_asset_id":        sourceAssetID,
			"source_asset_sha256":    sourceSHA,
			"canvas":                 document.Source.Canvas,
			"node_count":             nodeCount,
		},
		SourceTurnID: ec.TurnID,
		SourceCallID: ec.CallID,
	}
	layoutRef := assets.Ref{
		ID:                    layoutAssetID,
		Kind:                  "file",
		URI:                   assets.WorkspaceURI(workspaceID, persisted.LayoutPath, nil),
		DisplayName:           "layout.json",
		WorkspaceID:           workspaceID,
		WorkspaceRelativePath: persisted.LayoutPath,
		AbsolutePath:          layoutAbs,
		MIMEType:              "application/json",
		Metadata: map[string]any{
			"asset_role":             "layout_json",
			"layout_artifact_id":     artifactID,
			"layout_revision":        persisted.Revision,
			"layout_revision_sha256": revisionSHA,
			"source_asset_id":        sourceAssetID,
			"source_asset_sha256":    sourceSHA,
			"canvas":                 document.Source.Canvas,
			"node_count":             nodeCount,
		},
		SourceTurnID: ec.TurnID,
		SourceCallID: ec.CallID,
	}

	verb := "Persisted"
	if !persisted.Created {
		verb = "Reused"
	}
	content := fmt.Sprintf("%s ui_layout revision %d for %s (%d nodes, overlay %dx%d, canonical layout.json).",
		verb, persisted.Revision, sourceRel, nodeCount, overlayWidth, overlayHeight)
	return tools.ToolResult{Content: content, Output: outputJSON, Assets: []assets.Ref{overlayRef, layoutRef}}, nil
}

func layoutUsesEvidence(document layoutDocument) bool {
	nodes := []layoutNode{document.Root}
	for len(nodes) > 0 {
		node := nodes[len(nodes)-1]
		nodes = nodes[:len(nodes)-1]
		if len(node.EvidenceIDs) > 0 {
			return true
		}
		nodes = append(nodes, node.Children...)
	}
	for _, assumption := range document.Assumptions {
		var decoded struct {
			EvidenceIDs []string `json:"evidence_ids"`
		}
		if err := json.Unmarshal(assumption, &decoded); err == nil && len(decoded.EvidenceIDs) > 0 {
			return true
		}
	}
	return false
}

func decodeStrict(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return fmt.Errorf("input is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func resolveWorkspaceFile(root, inputPath, field string) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", coded("unavailable", "ui_layout requires a workspace root")
	}
	pathValue := strings.TrimSpace(inputPath)
	if filepath.IsAbs(pathValue) || strings.HasPrefix(pathValue, "/") || strings.Contains(pathValue, "\\") {
		return "", "", coded("blocked", "ui_layout "+field+" must be workspace-relative and use forward slashes")
	}
	if strings.IndexFunc(pathValue, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", "", coded("blocked", "ui_layout "+field+" contains control characters")
	}
	clean := filepath.Clean(filepath.FromSlash(pathValue))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", coded("blocked", "ui_layout "+field+" escapes the workspace")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	target := workspace.ResolveToolPath(rootAbs, clean)
	switch workspace.ClassifyPath(rootAbs, target) {
	case workspace.PathInsideWorkspace:
	case workspace.PathManagedWorktree:
		return "", "", coded("blocked", "ui_layout cannot read the managed worktree tree")
	default:
		return "", "", coded("blocked", "ui_layout "+field+" escapes the workspace")
	}
	target, err = workspace.CanonicalPath(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", coded("not_found", "ui_layout "+field+" was not found: "+inputPath)
		}
		return "", "", coded("blocked", "ui_layout "+field+" could not be resolved: "+err.Error())
	}
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", coded("not_found", "ui_layout "+field+" was not found: "+inputPath)
		}
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", coded("invalid_arguments", "ui_layout "+field+" must be a regular file")
	}
	rootCanonical, err := workspace.CanonicalPath(rootAbs)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(rootCanonical, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", coded("blocked", "ui_layout "+field+" escapes the workspace")
	}
	return target, filepath.ToSlash(rel), nil
}

func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, coded("not_found", "ui_layout file was not found")
		}
		return nil, err
	}
	defer file.Close()
	if maxBytes > 0 {
		data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxBytes {
			return nil, coded("too_large", "ui_layout input exceeds the document budget")
		}
		return data, nil
	}
	return io.ReadAll(file)
}

func sniffMIMEType(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	default:
		return ""
	}
}

func decodeImage(data []byte, mimeType string) (image.Image, error) {
	switch mimeType {
	case "image/png":
		src, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if src.Bounds().Dx() <= 0 || src.Bounds().Dy() <= 0 {
			return nil, fmt.Errorf("PNG has invalid dimensions")
		}
		return src, nil
	case "image/jpeg":
		src, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if src.Bounds().Dx() <= 0 || src.Bounds().Dy() <= 0 {
			return nil, fmt.Errorf("JPEG has invalid dimensions")
		}
		return src, nil
	default:
		return nil, fmt.Errorf("unsupported image MIME type %q", mimeType)
	}
}

func failureFromError(err error) tools.ToolResult {
	var codedErr *codedError
	if errors.As(err, &codedErr) {
		return tools.FailureResult(codedErr.kind, codedErr.message)
	}
	return tools.FailureResult("runtime", err.Error())
}

var (
	_ tools.Tool            = (*Tool)(nil)
	_ tools.BindingProvider = (*Tool)(nil)
)
