package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"code-agent/internal/uicontract"
	"code-agent/internal/workspace"
	"code-agent/schemas"
)

// Artifact metadata keys for the P1-F preview lineage layer. Metadata stays
// non-behavioral: the lineage is audit/replay data, not a render or security
// instruction.
const (
	MetadataPreviewMode = "preview_mode"
	MetadataLineage     = "ui_preview_lineage"
	LineageSchema       = "ui_preview_lineage.v2"
	MaxLineageBytes     = 8 << 10
)

// PreviewMode classifies who a preview revision is for. Adaptation previews are
// derived from a reviewed ui_layout.v1 revision and must carry lineage. Explore
// previews are standalone and must not.
type PreviewMode string

const (
	ModeAdaptation PreviewMode = "adaptation"
	ModeExplore    PreviewMode = "explore"
)

// Lineage error kinds are tool-facing failure kinds. They never reach the
// artifact error schema because a rejected lineage prevents artifact creation.
const (
	LineageRequired    = "lineage_required"
	LineageInvalid     = "lineage_invalid"
	LineageMismatch    = "lineage_mismatch"
	LineageUnavailable = "lineage_unavailable"
)

// LineageError is a typed lineage validation failure.
type LineageError struct {
	Kind    string
	Message string
}

func (e *LineageError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func lineageError(kind, format string, args ...any) error {
	return &LineageError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func lineageErrorKind(err error) string {
	if e, ok := err.(*LineageError); ok {
		return e.Kind
	}
	return LineageInvalid
}

// Lineage is the executable ui_preview_lineage.v2 record. It binds a preview
// revision to one reviewed layout revision, one source image, one target, and
// one UI state.
type Lineage struct {
	Schema               string              `json:"schema"`
	LayoutArtifactID     string              `json:"layout_artifact_id"`
	LayoutRevision       int64               `json:"layout_revision"`
	LayoutRevisionSHA256 string              `json:"layout_revision_sha256"`
	SourceAssetID        string              `json:"source_asset_id"`
	SourceAssetSHA256    string              `json:"source_asset_sha256"`
	EvidenceSHA256       *string             `json:"evidence_sha256"`
	Approval             LineageApproval     `json:"approval"`
	Target               string              `json:"target"`
	State                string              `json:"state"`
	TargetMatrix         LineageTargetMatrix `json:"target_matrix"`
}

type LineageApproval struct {
	State       string `json:"state"`
	EvidenceRef string `json:"evidence_ref,omitempty"`
}

type LineageTargetMatrix struct {
	ID      string   `json:"id"`
	Targets []string `json:"targets"`
}

func (l *LineageApproval) UnmarshalJSON(data []byte) error {
	type alias LineageApproval
	var out alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return err
	}
	*l = LineageApproval(out)
	return nil
}

func (m *LineageTargetMatrix) UnmarshalJSON(data []byte) error {
	type alias LineageTargetMatrix
	var out alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return err
	}
	*m = LineageTargetMatrix(out)
	return nil
}

func (l *Lineage) UnmarshalJSON(data []byte) error {
	type alias Lineage
	var out alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return err
	}
	*l = Lineage(out)
	return nil
}

// DecodeLineage validates raw bytes against the frozen lineage schema and its
// cross-field rules, then returns the typed record.
func DecodeLineage(data []byte) (*Lineage, error) {
	if len(bytes.TrimSpace(data)) == 0 || string(bytes.TrimSpace(data)) == "null" {
		return nil, lineageError(LineageRequired, "ui_preview_lineage.v2 is required")
	}
	if len(data) > MaxLineageBytes {
		return nil, lineageError(LineageInvalid, "ui_preview_lineage.v2 exceeds the %d-byte budget", MaxLineageBytes)
	}
	if err := uicontract.ValidateAgainstSchema(schemas.UIPreviewLineageV2, data); err != nil {
		return nil, lineageError(LineageInvalid, "ui_preview_lineage.v2 is invalid: %v", err)
	}
	if err := uicontract.ValidatePreviewLineage(data); err != nil {
		return nil, lineageError(LineageInvalid, "ui_preview_lineage.v2 is invalid: %v", err)
	}
	var lineage Lineage
	if err := json.Unmarshal(data, &lineage); err != nil {
		return nil, lineageError(LineageInvalid, "ui_preview_lineage.v2 could not be decoded: %v", err)
	}
	if lineage.EvidenceSHA256 != nil {
		trimmed := strings.TrimSpace(*lineage.EvidenceSHA256)
		if trimmed == "" {
			lineage.EvidenceSHA256 = nil
		} else {
			lineage.EvidenceSHA256 = &trimmed
		}
	}
	return &lineage, nil
}

// Validate re-validates the typed record through the same schema path used for
// wire input, so metadata cannot be accepted with fields the schema rejects.
func (l *Lineage) Validate() error {
	if l == nil {
		return lineageError(LineageRequired, "ui_preview_lineage.v2 is required")
	}
	data, err := json.Marshal(l)
	if err != nil {
		return lineageError(LineageInvalid, "ui_preview_lineage.v2 could not be encoded: %v", err)
	}
	_, err = DecodeLineage(data)
	return err
}

// ParsePreviewMode accepts the explicit mode strings. An empty value is handled
// by the caller so the tool and transport can choose different defaults.
func ParsePreviewMode(value string) (PreviewMode, bool) {
	switch PreviewMode(strings.TrimSpace(value)) {
	case ModeAdaptation:
		return ModeAdaptation, true
	case ModeExplore:
		return ModeExplore, true
	default:
		return "", false
	}
}

// ValidateLineageForPreview checks the lineage against the persisted layout
// revision, the current source image bytes, and the bound evidence bytes before
// any preview artifact is created.
func ValidateLineageForPreview(ctx context.Context, workspaceRoot string, lineage *Lineage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := lineage.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return lineageError(LineageUnavailable, "preview lineage requires a workspace root")
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return lineageError(LineageUnavailable, "preview lineage workspace could not be resolved")
	}

	revisionDir := filepath.Join(root, ".codeagent", "assets", "ui-layout", lineage.LayoutArtifactID, layoutRevisionName(lineage.LayoutRevision))
	rootCanonical, err := workspace.CanonicalPath(root)
	if err != nil {
		return lineageError(LineageUnavailable, "preview lineage workspace could not be canonicalized")
	}
	revisionCanonical, err := workspace.CanonicalPath(revisionDir)
	if err != nil {
		return lineageError(LineageUnavailable, "layout revision path is unavailable: %v", err)
	}
	switch workspace.ClassifyPath(rootCanonical, revisionCanonical) {
	case workspace.PathInsideWorkspace:
	default:
		return lineageError(LineageUnavailable, "layout revision path is outside the workspace")
	}
	revisionDir = revisionCanonical
	metadataBytes, err := readRegularWorkspaceFile(revisionDir, "metadata.json")
	if err != nil {
		return lineageError(LineageUnavailable, "layout revision metadata is unavailable: %v", err)
	}
	layoutBytes, err := readRegularWorkspaceFile(revisionDir, "layout.json")
	if err != nil {
		return lineageError(LineageUnavailable, "layout revision document is unavailable: %v", err)
	}

	var metadata layoutRevisionMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return lineageError(LineageInvalid, "layout revision metadata is invalid: %v", err)
	}
	if metadata.Kind != "ui_layout_revision" {
		return lineageError(LineageMismatch, "layout revision metadata kind is not ui_layout_revision")
	}
	if metadata.LayoutArtifactID != lineage.LayoutArtifactID || metadata.LayoutRevision != int(lineage.LayoutRevision) {
		return lineageError(LineageMismatch, "layout revision identity does not match lineage")
	}
	layoutDigest := digestHex(layoutBytes)
	if metadata.LayoutSHA256 != layoutDigest {
		return lineageError(LineageMismatch, "layout revision bytes do not match the persisted layout digest")
	}
	if metadata.LayoutRevisionSHA256 != "sha256:"+layoutDigest {
		return lineageError(LineageMismatch, "layout revision digest does not match the persisted document")
	}
	if metadata.LayoutRevisionSHA256 != lineage.LayoutRevisionSHA256 {
		return lineageError(LineageMismatch, "lineage layout_revision_sha256 does not match the persisted layout revision")
	}
	if metadata.SourceAssetID != lineage.SourceAssetID || metadata.SourceAssetSHA256 != lineage.SourceAssetSHA256 {
		return lineageError(LineageMismatch, "lineage source identity does not match the layout revision")
	}
	if !evidenceDigestMatches(lineage.EvidenceSHA256, metadata.EvidenceSHA256) {
		return lineageError(LineageMismatch, "lineage evidence_sha256 does not match the layout revision")
	}

	var layout lineageLayoutDocument
	if err := json.Unmarshal(layoutBytes, &layout); err != nil {
		return lineageError(LineageInvalid, "layout revision document is invalid: %v", err)
	}
	if layout.Source.AssetID != lineage.SourceAssetID || layout.Source.SourceAssetSHA256 != lineage.SourceAssetSHA256 {
		return lineageError(LineageMismatch, "layout source identity does not match lineage")
	}
	if !evidenceDigestMatches(lineage.EvidenceSHA256, layout.Source.EvidenceSHA256) {
		return lineageError(LineageMismatch, "layout evidence digest does not match lineage")
	}
	if lineage.EvidenceSHA256 != nil && layout.Source.EvidenceSchema != "" && layout.Source.EvidenceSchema != "ui_evidence.v1" {
		return lineageError(LineageMismatch, "layout evidence_schema must be ui_evidence.v1 when lineage binds evidence")
	}
	if _, ok := layout.Responsive.Targets[lineage.Target]; !ok {
		return lineageError(LineageMismatch, "lineage target %q is not declared by the layout revision", lineage.Target)
	}
	if !containsString(layout.Screen.States, lineage.State) {
		return lineageError(LineageMismatch, "lineage state %q is not declared by the layout revision", lineage.State)
	}

	if metadata.SourcePath == "" {
		return lineageError(LineageMismatch, "layout revision does not record its source path")
	}
	sourceBytes, err := readWorkspaceRelativeFile(root, metadata.SourcePath)
	if err != nil {
		return lineageError(LineageUnavailable, "layout source image is unavailable: %v", err)
	}
	if digestHex(sourceBytes) != lineage.SourceAssetSHA256 {
		return lineageError(LineageMismatch, "current source image does not match lineage source_asset_sha256")
	}

	if lineage.EvidenceSHA256 != nil {
		if metadata.EvidencePath != "" {
			evidenceBytes, err := readWorkspaceRelativeFile(root, metadata.EvidencePath)
			if err != nil {
				return lineageError(LineageUnavailable, "layout evidence file is unavailable: %v", err)
			}
			if digestHex(evidenceBytes) != *lineage.EvidenceSHA256 {
				return lineageError(LineageMismatch, "current evidence file does not match lineage evidence_sha256")
			}
		}
	}
	return nil
}

type layoutRevisionMetadata struct {
	Kind                 string `json:"kind"`
	LayoutArtifactID     string `json:"layout_artifact_id"`
	LayoutRevision       int    `json:"layout_revision"`
	LayoutRevisionSHA256 string `json:"layout_revision_sha256"`
	LayoutSHA256         string `json:"layout_sha256"`
	SourcePath           string `json:"source_path"`
	SourceAssetID        string `json:"source_asset_id"`
	SourceAssetSHA256    string `json:"source_asset_sha256"`
	EvidencePath         string `json:"evidence_path"`
	EvidenceSHA256       string `json:"evidence_sha256"`
}

type lineageLayoutDocument struct {
	Source struct {
		AssetID           string `json:"asset_id"`
		SourceAssetSHA256 string `json:"source_asset_sha256"`
		EvidenceSchema    string `json:"evidence_schema"`
		EvidenceSHA256    string `json:"evidence_sha256"`
	} `json:"source"`
	Screen struct {
		States []string `json:"states"`
	} `json:"screen"`
	Responsive struct {
		Targets map[string]json.RawMessage `json:"targets"`
	} `json:"responsive"`
}

func layoutRevisionName(revision int64) string {
	return fmt.Sprintf("revision-%06d", revision)
}

func evidenceDigestMatches(lineage *string, layout string) bool {
	layout = strings.TrimSpace(layout)
	if lineage == nil {
		return layout == ""
	}
	return strings.TrimSpace(*lineage) == layout && layout != ""
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func readRegularWorkspaceFile(dir, name string) ([]byte, error) {
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

func readWorkspaceRelativeFile(root, rel string) ([]byte, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || strings.Contains(rel, "\\") || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("path must be workspace-relative")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path escapes the workspace")
	}
	target := workspace.ResolveToolPath(root, clean)
	target, err := workspace.CanonicalPath(target)
	if err != nil {
		return nil, err
	}
	rootCanonical, err := workspace.CanonicalPath(root)
	if err != nil {
		return nil, err
	}
	switch workspace.ClassifyPath(rootCanonical, target) {
	case workspace.PathInsideWorkspace:
	default:
		return nil, fmt.Errorf("path escapes the workspace")
	}
	info, err := os.Lstat(target)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	return os.ReadFile(target)
}

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// normalizeDraftMode treats an omitted transport mode as explore. The
// ui_preview tool applies the stricter adaptation default itself, so standalone
// client drafts remain explicitly classified as explore previews.
func normalizeDraftMode(value PreviewMode) (PreviewMode, *Error) {
	switch value {
	case "":
		return ModeExplore, nil
	case ModeAdaptation:
		return ModeAdaptation, nil
	case ModeExplore:
		return ModeExplore, nil
	default:
		return "", NewError(CodeInvalidArtifact, "Preview mode is unsupported.", false)
	}
}

// validateDraftLineage enforces the mode/lineage coupling before persistence.
func validateDraftLineage(mode PreviewMode, lineage *Lineage) *Error {
	switch mode {
	case ModeAdaptation:
		if lineage == nil {
			return NewError(CodeInvalidArtifact, "Adaptation preview requires ui_preview_lineage.v2.", false)
		}
		if err := lineage.Validate(); err != nil {
			return NewError(CodeInvalidArtifact, "Preview lineage is invalid: "+err.Error(), false)
		}
	case ModeExplore:
		if lineage != nil {
			return NewError(CodeInvalidArtifact, "Explore preview must not carry lineage.", false)
		}
	default:
		return NewError(CodeInvalidArtifact, "Preview mode is unsupported.", false)
	}
	return nil
}

// validateArtifactLineage validates lineage metadata on persisted artifacts and
// replay snapshots. Legacy artifacts without preview_mode remain readable.
func validateArtifactLineage(metadata map[string]any) *Error {
	modeValue, hasMode := metadata[MetadataPreviewMode]
	lineageValue, hasLineage := metadata[MetadataLineage]
	if !hasMode {
		if hasLineage {
			return NewError(CodeInvalidArtifact, "Preview lineage requires preview_mode metadata.", false)
		}
		return nil
	}
	modeText, ok := modeValue.(string)
	if !ok {
		return NewError(CodeInvalidArtifact, "Preview mode metadata is invalid.", false)
	}
	mode, ok := ParsePreviewMode(modeText)
	if !ok {
		return NewError(CodeInvalidArtifact, "Preview mode metadata is invalid.", false)
	}
	switch mode {
	case ModeAdaptation:
		if !hasLineage {
			return NewError(CodeInvalidArtifact, "Adaptation preview metadata requires lineage.", false)
		}
		data, err := json.Marshal(lineageValue)
		if err != nil {
			return NewError(CodeInvalidArtifact, "Preview lineage metadata is invalid.", false)
		}
		if len(data) > MaxLineageBytes {
			return NewError(CodeInvalidArtifact, "Preview lineage metadata exceeds the budget.", false)
		}
		if _, err := DecodeLineage(data); err != nil {
			return NewError(CodeInvalidArtifact, "Preview lineage metadata is invalid: "+err.Error(), false)
		}
	case ModeExplore:
		if hasLineage {
			return NewError(CodeInvalidArtifact, "Explore preview must not carry lineage metadata.", false)
		}
	}
	return nil
}
