package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"code-agent/internal/preview"
	"code-agent/internal/uicontract"
	"code-agent/internal/workspace"
	"code-agent/schemas"
)

var (
	matrixIDPattern   = regexp.MustCompile(`^matrix_[A-Za-z0-9._:-]{1,180}$`)
	layoutIDPattern   = regexp.MustCompile(`^layout_[A-Za-z0-9._-]{1,180}$`)
	publicIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,191}$`)
	layoutBaseDir     = filepath.Join(".codeagent", "assets", "ui-layout")
	matrixRevisionFmt = "revision-%06d"
)

// allowedTargets mirrors the ui_preview_lineage.v2 target enum. A layout may
// declare other responsive targets, but a matrix cannot carry them.
var allowedTargets = map[string]struct{}{
	"duo_folded":       {},
	"duo_unfolded":     {},
	"iphone_portrait":  {},
	"iphone_landscape": {},
	"ipad_portrait":    {},
	"ipad_landscape":   {},
}

// LayoutRef selects the reviewed layout revision a matrix aggregates.
type LayoutRef struct {
	ArtifactID string
	Revision   int64
}

// CellRef points one target/state cell at an independent preview revision, or
// records that the cell failed before a revision existed.
type CellRef struct {
	Target            string
	State             string
	PreviewArtifactID string
	PreviewRevision   int64
	Error             *EntryError
}

// Request describes one matrix build. An empty Targets or States selection
// means every target or state the layout revision declares.
type Request struct {
	SessionID     string
	MatrixID      string
	WorkspaceRoot string
	Layout        LayoutRef
	Targets       []string
	States        []string
	Cells         []CellRef
}

// Builder resolves preview revisions from the preview store.
type Builder struct {
	Store preview.Store
}

func NewBuilder(store preview.Store) *Builder { return &Builder{Store: store} }

// Build aggregates the requested cells into a sealed ui_preview_matrix.v1
// document. Per-cell preview problems are recorded as failed entries; only a
// request or layout problem fails the build.
func (b *Builder) Build(ctx context.Context, request Request) (Matrix, error) {
	if err := ctx.Err(); err != nil {
		return Matrix{}, err
	}
	if b == nil || b.Store == nil {
		return Matrix{}, newError(CodeStoreUnavailable, "preview store is unavailable")
	}
	if strings.TrimSpace(request.WorkspaceRoot) == "" {
		return Matrix{}, newError(CodeInvalid, "matrix requires a workspace root")
	}
	if !publicIDPattern.MatchString(request.SessionID) {
		return Matrix{}, newError(CodeInvalid, "matrix session identity is invalid")
	}
	if !matrixIDPattern.MatchString(request.MatrixID) {
		return Matrix{}, newError(CodeInvalid, "matrix_id must match matrix_<id>")
	}
	if !layoutIDPattern.MatchString(request.Layout.ArtifactID) || request.Layout.Revision < 1 {
		return Matrix{}, newError(CodeInvalid, "matrix layout reference is invalid")
	}
	root, err := filepath.Abs(request.WorkspaceRoot)
	if err != nil {
		return Matrix{}, newError(CodeInvalid, "matrix workspace could not be resolved")
	}
	layout, err := loadLayout(ctx, root, request.Layout)
	if err != nil {
		return Matrix{}, err
	}
	targets, err := selectTargets(layout.targets, request.Targets)
	if err != nil {
		return Matrix{}, err
	}
	states, err := selectStates(layout.states, request.States)
	if err != nil {
		return Matrix{}, err
	}
	cells, err := indexCells(targets, states, request.Cells)
	if err != nil {
		return Matrix{}, err
	}

	document := Matrix{
		Schema:   Schema,
		MatrixID: request.MatrixID,
		SourceAsset: SourceIdentity{
			AssetID: layout.sourceAssetID,
			SHA256:  layout.sourceAssetSHA256,
		},
		Layout:         layout.identity,
		EvidenceSHA256: layout.evidenceSHA256,
		TargetMatrix:   TargetMatrix{ID: request.MatrixID, Targets: targets},
		States:         states,
	}
	document.Entries = make([]Entry, 0, len(targets)*len(states))
	coverage := Coverage{Targets: len(targets), States: len(states), Expected: len(targets) * len(states)}
	for _, target := range targets {
		for _, state := range states {
			if err := ctx.Err(); err != nil {
				return Matrix{}, err
			}
			entry := b.resolveCell(ctx, request, document, target, state, cells[target+"\x00"+state])
			switch entry.Status {
			case StatusCompleted:
				coverage.Completed++
			case StatusFailed:
				coverage.Failed++
			case StatusMissing:
				coverage.Missing++
			}
			document.Entries = append(document.Entries, entry)
		}
	}
	coverage.Complete = coverage.Completed == coverage.Expected
	document.Coverage = coverage
	if err := document.Seal(); err != nil {
		return Matrix{}, err
	}
	if err := document.Validate(); err != nil {
		return Matrix{}, err
	}
	return document, nil
}

// layoutState is what the persisted layout revision declares and what the
// matrix must agree with.
type layoutState struct {
	identity          LayoutIdentity
	sourceAssetID     string
	sourceAssetSHA256 string
	evidenceSHA256    *string
	targets           []string
	states            []string
}

type layoutRevisionMetadata struct {
	Kind                 string `json:"kind"`
	LayoutArtifactID     string `json:"layout_artifact_id"`
	LayoutRevision       int64  `json:"layout_revision"`
	LayoutRevisionSHA256 string `json:"layout_revision_sha256"`
	LayoutSHA256         string `json:"layout_sha256"`
	SourcePath           string `json:"source_path"`
	SourceAssetID        string `json:"source_asset_id"`
	SourceAssetSHA256    string `json:"source_asset_sha256"`
	EvidencePath         string `json:"evidence_path"`
	EvidenceSHA256       string `json:"evidence_sha256"`
}

type layoutDocument struct {
	Schema string `json:"schema"`
	Source struct {
		AssetID           string `json:"asset_id"`
		SourceAssetSHA256 string `json:"source_asset_sha256"`
		EvidenceSHA256    string `json:"evidence_sha256"`
	} `json:"source"`
	Screen struct {
		States []string `json:"states"`
	} `json:"screen"`
	Responsive struct {
		Targets map[string]json.RawMessage `json:"targets"`
	} `json:"responsive"`
}

func loadLayout(ctx context.Context, root string, ref LayoutRef) (layoutState, error) {
	if err := ctx.Err(); err != nil {
		return layoutState{}, err
	}
	rootCanonical, err := workspace.CanonicalPath(root)
	if err != nil {
		return layoutState{}, newError(CodeLayoutUnavailable, "layout workspace could not be canonicalized")
	}
	revisionDir := filepath.Join(rootCanonical, layoutBaseDir, ref.ArtifactID, fmt.Sprintf(matrixRevisionFmt, ref.Revision))
	revisionCanonical, err := workspace.CanonicalPath(revisionDir)
	if err != nil {
		return layoutState{}, newErrorWithDiagnostic(CodeLayoutUnavailable, "layout revision path is unavailable", err)
	}
	if workspace.ClassifyPath(rootCanonical, revisionCanonical) != workspace.PathInsideWorkspace {
		return layoutState{}, newError(CodeLayoutUnavailable, "layout revision path is outside the workspace")
	}
	metadataBytes, err := readRegularFile(revisionCanonical, "metadata.json")
	if err != nil {
		return layoutState{}, newErrorWithDiagnostic(CodeLayoutUnavailable, "layout revision metadata is unavailable", err)
	}
	layoutBytes, err := readRegularFile(revisionCanonical, "layout.json")
	if err != nil {
		return layoutState{}, newErrorWithDiagnostic(CodeLayoutUnavailable, "layout revision document is unavailable", err)
	}
	var metadata layoutRevisionMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return layoutState{}, newError(CodeLayoutInvalid, "layout revision metadata is invalid: %v", err)
	}
	if metadata.Kind != "ui_layout_revision" {
		return layoutState{}, newError(CodeLayoutMismatch, "layout revision metadata kind is not ui_layout_revision")
	}
	if metadata.LayoutArtifactID != ref.ArtifactID || metadata.LayoutRevision != ref.Revision {
		return layoutState{}, newError(CodeLayoutMismatch, "layout revision identity does not match the request")
	}
	layoutDigest := strings.TrimPrefix(preview.DigestBytes(layoutBytes), "sha256:")
	if metadata.LayoutSHA256 != layoutDigest {
		return layoutState{}, newError(CodeLayoutMismatch, "layout revision bytes do not match the persisted layout digest")
	}
	if metadata.LayoutRevisionSHA256 != "sha256:"+layoutDigest {
		return layoutState{}, newError(CodeLayoutMismatch, "layout revision digest does not match the persisted document")
	}
	if err := uicontract.ValidateAgainstSchema(schemas.UILayoutV1, layoutBytes); err != nil {
		return layoutState{}, newError(CodeLayoutInvalid, "layout revision document is invalid: %v", err)
	}
	var document layoutDocument
	if err := json.Unmarshal(layoutBytes, &document); err != nil {
		return layoutState{}, newError(CodeLayoutInvalid, "layout revision document could not be decoded: %v", err)
	}
	if document.Schema != "ui_layout.v1" {
		return layoutState{}, newError(CodeLayoutInvalid, "layout revision schema is not ui_layout.v1")
	}
	if metadata.SourceAssetID != document.Source.AssetID || metadata.SourceAssetSHA256 != document.Source.SourceAssetSHA256 {
		return layoutState{}, newError(CodeLayoutMismatch, "layout revision source identity does not match its document")
	}
	if !strings.HasPrefix(document.Source.AssetID, "asset_") || !isLowerHexSHA256(document.Source.SourceAssetSHA256) {
		return layoutState{}, newError(CodeLayoutInvalid, "layout revision source identity is invalid")
	}
	evidenceText := strings.TrimSpace(metadata.EvidenceSHA256)
	var evidence *string
	if evidenceText != "" {
		if !isLowerHexSHA256(evidenceText) {
			return layoutState{}, newError(CodeLayoutInvalid, "layout revision evidence digest is invalid")
		}
		value := evidenceText
		evidence = &value
	}
	if strings.TrimSpace(document.Source.EvidenceSHA256) != evidenceText {
		return layoutState{}, newError(CodeLayoutMismatch, "layout revision evidence digest does not match its document")
	}
	if len(document.Screen.States) == 0 {
		return layoutState{}, newError(CodeLayoutInvalid, "layout revision declares no screen states")
	}
	targets := make([]string, 0, len(document.Responsive.Targets))
	for target := range document.Responsive.Targets {
		if _, ok := allowedTargets[target]; !ok {
			return layoutState{}, newError(CodeLayoutInvalid, "layout revision declares unsupported target %q", target)
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return layoutState{}, newError(CodeLayoutInvalid, "layout revision declares no responsive targets")
	}
	sort.Strings(targets)
	return layoutState{
		identity: LayoutIdentity{
			ArtifactID:     metadata.LayoutArtifactID,
			Revision:       metadata.LayoutRevision,
			RevisionSHA256: metadata.LayoutRevisionSHA256,
		},
		sourceAssetID:     document.Source.AssetID,
		sourceAssetSHA256: document.Source.SourceAssetSHA256,
		evidenceSHA256:    evidence,
		targets:           targets,
		states:            append([]string(nil), document.Screen.States...),
	}, nil
}

func readRegularFile(dir, name string) ([]byte, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return os.ReadFile(path)
}

func selectTargets(declared, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return append([]string(nil), declared...), nil
	}
	seen := make(map[string]struct{}, len(requested))
	for _, target := range requested {
		if !containsString(declared, target) {
			return nil, newError(CodeInvalid, "target %q is not declared by the layout revision", target)
		}
		if _, ok := seen[target]; ok {
			return nil, newError(CodeInvalid, "target %q was selected more than once", target)
		}
		seen[target] = struct{}{}
	}
	selected := make([]string, 0, len(seen))
	for _, target := range declared {
		if _, ok := seen[target]; ok {
			selected = append(selected, target)
		}
	}
	return selected, nil
}

func selectStates(declared, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return append([]string(nil), declared...), nil
	}
	seen := make(map[string]struct{}, len(requested))
	for _, state := range requested {
		if !containsString(declared, state) {
			return nil, newError(CodeInvalid, "state %q is not declared by the layout revision", state)
		}
		if _, ok := seen[state]; ok {
			return nil, newError(CodeInvalid, "state %q was selected more than once", state)
		}
		seen[state] = struct{}{}
	}
	selected := make([]string, 0, len(seen))
	for _, state := range declared {
		if _, ok := seen[state]; ok {
			selected = append(selected, state)
		}
	}
	return selected, nil
}

func indexCells(targets, states []string, refs []CellRef) (map[string]CellRef, error) {
	if len(targets)*len(states) > MaxEntries {
		return nil, newError(CodeInvalid, "matrix declares %d cells, more than the %d-cell budget", len(targets)*len(states), MaxEntries)
	}
	targetSet := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetSet[target] = struct{}{}
	}
	stateSet := make(map[string]struct{}, len(states))
	for _, state := range states {
		stateSet[state] = struct{}{}
	}
	cells := make(map[string]CellRef, len(refs))
	artifactIDs := make(map[string]struct{}, len(refs))
	revisions := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if _, ok := targetSet[ref.Target]; !ok {
			return nil, newError(CodeInvalid, "cell target %q is not part of the matrix", ref.Target)
		}
		if _, ok := stateSet[ref.State]; !ok {
			return nil, newError(CodeInvalid, "cell state %q is not part of the matrix", ref.State)
		}
		key := ref.Target + "\x00" + ref.State
		if _, ok := cells[key]; ok {
			return nil, newError(CodeInvalid, "cell %s/%s was declared more than once", ref.Target, ref.State)
		}
		hasPreview := strings.TrimSpace(ref.PreviewArtifactID) != ""
		if ref.Error != nil {
			if hasPreview || ref.PreviewRevision != 0 {
				return nil, newError(CodeInvalid, "cell %s/%s declares a failure and a preview revision", ref.Target, ref.State)
			}
			if err := validateEntryError(*ref.Error); err != nil {
				return nil, newError(CodeInvalid, "cell %s/%s: %v", ref.Target, ref.State, err)
			}
		} else if hasPreview {
			if ref.PreviewRevision < 1 {
				return nil, newError(CodeInvalid, "cell %s/%s preview revision must be at least 1", ref.Target, ref.State)
			}
			if _, ok := artifactIDs[ref.PreviewArtifactID]; ok {
				return nil, newError(CodeInvalid, "preview artifact %s is referenced by more than one cell", ref.PreviewArtifactID)
			}
			artifactIDs[ref.PreviewArtifactID] = struct{}{}
			revisionKey := fmt.Sprintf("%s@%d", ref.PreviewArtifactID, ref.PreviewRevision)
			if _, ok := revisions[revisionKey]; ok {
				return nil, newError(CodeInvalid, "preview revision %s is referenced by more than one cell", revisionKey)
			}
			revisions[revisionKey] = struct{}{}
		} else if ref.PreviewRevision != 0 {
			return nil, newError(CodeInvalid, "cell %s/%s has a revision without a preview artifact", ref.Target, ref.State)
		}
		cells[key] = ref
	}
	return cells, nil
}

func (b *Builder) resolveCell(ctx context.Context, request Request, document Matrix, target, state string, ref CellRef) Entry {
	entry := Entry{Target: target, State: state}
	if ref.Error != nil {
		entry.Status = StatusFailed
		entry.Error = &EntryError{Code: ref.Error.Code, Message: ref.Error.Message}
		return entry
	}
	if strings.TrimSpace(ref.PreviewArtifactID) == "" {
		entry.Status = StatusMissing
		return entry
	}
	artifact, err := b.Store.GetArtifact(ctx, request.SessionID, ref.PreviewArtifactID, ref.PreviewRevision)
	if err != nil {
		code, message := storeFailure(err)
		entry.Status = StatusFailed
		entry.Error = &EntryError{Code: code, Message: message}
		return entry
	}
	artifactID, revision, revisionSHA := artifact.ArtifactID, artifact.Revision, artifact.RevisionSHA256
	entry.PreviewArtifactID = &artifactID
	entry.PreviewRevision = &revision
	entry.PreviewRevisionSHA256 = &revisionSHA
	if mode, ok := previewMode(artifact); ok {
		entry.PreviewMode = &mode
	}
	fail := func(code, format string, args ...any) Entry {
		entry.Status = StatusFailed
		entry.Error = &EntryError{Code: code, Message: fmt.Sprintf(format, args...)}
		return entry
	}
	if artifact.Kind != preview.KindInteractiveHTML {
		return fail(string(preview.CodeInvalidArtifact), "matrix cells require interactive_html previews")
	}
	if entry.PreviewMode == nil || *entry.PreviewMode != string(preview.ModeAdaptation) {
		return fail(string(preview.CodeInvalidArtifact), "matrix cells require adaptation previews")
	}
	lineage, err := artifactLineage(artifact)
	if err != nil {
		var lineageErr *preview.LineageError
		if errors.As(err, &lineageErr) {
			return fail(lineageErr.Kind, "%s", lineageErr.Message)
		}
		return fail(preview.LineageInvalid, "preview lineage is invalid: %v", err)
	}
	if err := checkLineageBinding(document, entry.Target, entry.State, lineage); err != nil {
		return fail(preview.LineageMismatch, "%s", err.Error())
	}
	switch artifact.Status {
	case preview.StatusReady:
	case preview.StatusFailed, preview.StatusUnavailable:
		code := string(preview.CodeRenderFailed)
		message := "Preview revision failed."
		if artifact.Error != nil {
			if entryErrorCodeAllowed(string(artifact.Error.Code)) {
				code = string(artifact.Error.Code)
			}
			if strings.TrimSpace(artifact.Error.Message) != "" {
				message = artifact.Error.Message
			}
		}
		return fail(code, "%s", message)
	default:
		return fail(string(preview.CodeRenderFailed), "preview revision is not ready")
	}
	if artifact.Viewer == nil || artifact.Fallback == nil {
		return fail(string(preview.CodeInvalidArtifact), "ready preview revision is missing its viewer or fallback")
	}
	fallback, ok := artifact.FindAsset(artifact.Fallback.AssetID)
	if !ok || fallback.Role != preview.RoleFallback {
		return fail(string(preview.CodeAssetUnavailable), "preview fallback asset is unavailable")
	}
	entry.Status = StatusCompleted
	entry.Lineage = lineage
	entry.FallbackAsset = &FallbackAsset{
		AssetID:  fallback.AssetID,
		MIMEType: fallback.MIMEType,
		Width:    fallback.Width,
		Height:   fallback.Height,
		SHA256:   strings.TrimPrefix(fallback.SHA256, "sha256:"),
	}
	return entry
}

func previewMode(artifact preview.Artifact) (string, bool) {
	value, ok := artifact.Metadata[preview.MetadataPreviewMode]
	if !ok {
		return "", false
	}
	mode, ok := value.(string)
	if !ok {
		return "", false
	}
	if _, ok := preview.ParsePreviewMode(mode); !ok {
		return "", false
	}
	return mode, true
}

func artifactLineage(artifact preview.Artifact) (*preview.Lineage, error) {
	value, ok := artifact.Metadata[preview.MetadataLineage]
	if !ok {
		return nil, newError(CodeInvalid, "adaptation preview metadata is missing lineage")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return preview.DecodeLineage(data)
}

func checkLineageBinding(document Matrix, target, state string, lineage *preview.Lineage) error {
	if lineage.LayoutArtifactID != document.Layout.ArtifactID ||
		lineage.LayoutRevision != document.Layout.Revision ||
		lineage.LayoutRevisionSHA256 != document.Layout.RevisionSHA256 {
		return fmt.Errorf("lineage layout revision does not match the matrix layout revision")
	}
	if lineage.SourceAssetID != document.SourceAsset.AssetID || lineage.SourceAssetSHA256 != document.SourceAsset.SHA256 {
		return fmt.Errorf("lineage source asset does not match the matrix source asset")
	}
	if !sameEvidence(lineage.EvidenceSHA256, document.EvidenceSHA256) {
		return fmt.Errorf("lineage evidence does not match the matrix evidence")
	}
	if lineage.Target != target {
		return fmt.Errorf("lineage target %q does not match the cell target %q", lineage.Target, target)
	}
	if lineage.State != state {
		return fmt.Errorf("lineage state %q does not match the cell state %q", lineage.State, state)
	}
	if lineage.TargetMatrix.ID != document.MatrixID {
		return fmt.Errorf("lineage target_matrix.id %q does not match matrix %q", lineage.TargetMatrix.ID, document.MatrixID)
	}
	if !sameStringSet(lineage.TargetMatrix.Targets, document.TargetMatrix.Targets) {
		return fmt.Errorf("lineage target_matrix.targets do not match the matrix targets")
	}
	return nil
}

func storeFailure(err error) (string, string) {
	var previewErr *preview.Error
	if errors.As(err, &previewErr) {
		if entryErrorCodeAllowed(string(previewErr.Code)) {
			return string(previewErr.Code), previewErr.Message
		}
		return string(preview.CodeStoreUnavailable), previewErr.Message
	}
	return string(preview.CodeStoreUnavailable), "preview store is unavailable"
}
