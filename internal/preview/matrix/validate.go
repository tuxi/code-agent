package matrix

import (
	"bytes"
	"encoding/json"
	"strings"

	"code-agent/internal/preview"
	"code-agent/internal/uicontract"
	"code-agent/schemas"
)

// entryErrorCodes are the only failure kinds a cell may declare: the P1-F
// lineage kinds plus the frozen preview error codes. A matrix consumer can
// switch on this closed set.
var entryErrorCodes = map[string]struct{}{
	preview.LineageRequired:                {},
	preview.LineageInvalid:                 {},
	preview.LineageMismatch:                {},
	preview.LineageUnavailable:             {},
	string(preview.CodeUnsupported):        {},
	string(preview.CodeArtifactMissing):    {},
	string(preview.CodeInvalidArtifact):    {},
	string(preview.CodeViewerTooLarge):     {},
	string(preview.CodeViewerMIMEMismatch): {},
	string(preview.CodeDigestMismatch):     {},
	string(preview.CodeViewerFetchFailed):  {},
	string(preview.CodeSecurityViolation):  {},
	string(preview.CodeRenderFailed):       {},
	string(preview.CodeAssetUnavailable):   {},
	string(preview.CodeExpired):            {},
	string(preview.CodeRevisionConflict):   {},
	string(preview.CodeStoreUnavailable):   {},
}

func entryErrorCodeAllowed(code string) bool {
	_, ok := entryErrorCodes[code]
	return ok
}

// ValidateDocument validates one ui_preview_matrix.v1 document: the frozen JSON
// Schema plus the cross-field rules a consumer must be able to trust.
func ValidateDocument(data []byte) error {
	if err := uicontract.ValidateAgainstSchema(schemas.UIPreviewMatrixV1, data); err != nil {
		return newError(CodeInvalid, "matrix is invalid: %v", err)
	}
	var document Matrix
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return newError(CodeInvalid, "matrix is invalid: %v", err)
	}
	return validateMatrix(document)
}

func validateMatrix(m Matrix) error {
	if m.TargetMatrix.ID != m.MatrixID {
		return newError(CodeInvalid, "target_matrix.id %q does not match matrix_id", m.TargetMatrix.ID)
	}
	// Canonical target order makes two builds of the same inputs byte-identical.
	for i := 1; i < len(m.TargetMatrix.Targets); i++ {
		if m.TargetMatrix.Targets[i-1] >= m.TargetMatrix.Targets[i] {
			return newError(CodeInvalid, "target_matrix.targets must be sorted and unique")
		}
	}

	expected := len(m.TargetMatrix.Targets) * len(m.States)
	if len(m.Entries) != expected {
		return newError(CodeInvalid, "matrix declares %d cells but carries %d entries; missing cells cannot be omitted", expected, len(m.Entries))
	}
	coverage := Coverage{Targets: len(m.TargetMatrix.Targets), States: len(m.States), Expected: expected}
	seenArtifacts := make(map[string]struct{})
	for index, entry := range m.Entries {
		wantTarget := m.TargetMatrix.Targets[index/len(m.States)]
		wantState := m.States[index%len(m.States)]
		if entry.Target != wantTarget || entry.State != wantState {
			return newError(CodeInvalid, "entry %d is %s/%s, want %s/%s in canonical cell order", index, entry.Target, entry.State, wantTarget, wantState)
		}
		if err := validateEntry(m, entry); err != nil {
			return err
		}
		switch entry.Status {
		case StatusCompleted:
			coverage.Completed++
		case StatusFailed:
			coverage.Failed++
		case StatusMissing:
			coverage.Missing++
		}
		if entry.PreviewArtifactID != nil {
			// One cell owns one independent preview revision.
			if _, ok := seenArtifacts[*entry.PreviewArtifactID]; ok {
				return newError(CodeInvalid, "preview artifact %s is referenced by more than one cell", *entry.PreviewArtifactID)
			}
			seenArtifacts[*entry.PreviewArtifactID] = struct{}{}
		}
	}
	coverage.Complete = coverage.Completed == coverage.Expected
	if coverage != m.Coverage {
		return newError(CodeInvalid, "coverage %+v does not match entries %+v", m.Coverage, coverage)
	}
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	if !strings.EqualFold(m.MatrixSHA256, digest) {
		return newError(CodeInvalid, "matrix_sha256 does not match the document")
	}
	return nil
}

func validateEntry(m Matrix, entry Entry) error {
	cell := entry.Target + "/" + entry.State
	if entry.Error != nil {
		if err := validateEntryError(*entry.Error); err != nil {
			return err
		}
	}
	switch entry.Status {
	case StatusCompleted:
		if entry.PreviewArtifactID == nil || entry.PreviewRevision == nil || entry.PreviewRevisionSHA256 == nil || entry.PreviewMode == nil {
			return newError(CodeInvalid, "completed cell %s must carry a preview artifact identity", cell)
		}
		if *entry.PreviewMode != string(preview.ModeAdaptation) {
			return newError(CodeInvalid, "completed cell %s must be an adaptation preview", cell)
		}
		if entry.Lineage == nil {
			return newError(CodeInvalid, "completed cell %s requires ui_preview_lineage.v2", cell)
		}
		if err := validateEntryLineage(m, entry); err != nil {
			return err
		}
		if entry.FallbackAsset == nil {
			return newError(CodeInvalid, "completed cell %s requires a fallback asset identity", cell)
		}
		if entry.Error != nil {
			return newError(CodeInvalid, "completed cell %s must not carry an error", cell)
		}
	case StatusFailed:
		if entry.Error == nil {
			return newError(CodeInvalid, "failed cell %s requires an error", cell)
		}
		if entry.Lineage != nil || entry.FallbackAsset != nil {
			return newError(CodeInvalid, "failed cell %s must not carry lineage or a fallback", cell)
		}
		if (entry.PreviewArtifactID == nil) != (entry.PreviewRevision == nil) ||
			(entry.PreviewArtifactID == nil) != (entry.PreviewRevisionSHA256 == nil) ||
			(entry.PreviewArtifactID == nil) != (entry.PreviewMode == nil) {
			return newError(CodeInvalid, "failed cell %s has a partial preview identity", cell)
		}
	case StatusMissing:
		if entry.PreviewArtifactID != nil || entry.PreviewRevision != nil || entry.PreviewRevisionSHA256 != nil ||
			entry.PreviewMode != nil || entry.Lineage != nil || entry.FallbackAsset != nil || entry.Error != nil {
			return newError(CodeInvalid, "missing cell %s must not claim a preview, lineage, fallback, or error", cell)
		}
	default:
		return newError(CodeInvalid, "cell %s status %q is unsupported", cell, entry.Status)
	}
	return nil
}

func validateEntryError(entryErr EntryError) error {
	if !entryErrorCodeAllowed(entryErr.Code) {
		return newError(CodeInvalid, "error code %q is not a preview or lineage failure kind", entryErr.Code)
	}
	if strings.TrimSpace(entryErr.Message) == "" {
		return newError(CodeInvalid, "error message is required")
	}
	if len([]byte(entryErr.Message)) > MaxErrorTextBytes {
		return newError(CodeInvalid, "error message exceeds the %d-byte budget", MaxErrorTextBytes)
	}
	return nil
}

func validateEntryLineage(m Matrix, entry Entry) error {
	lineage := entry.Lineage
	cell := entry.Target + "/" + entry.State
	if err := lineage.Validate(); err != nil {
		return newError(CodeInvalid, "cell %s lineage is invalid: %v", cell, err)
	}
	if lineage.LayoutArtifactID != m.Layout.ArtifactID ||
		lineage.LayoutRevision != m.Layout.Revision ||
		lineage.LayoutRevisionSHA256 != m.Layout.RevisionSHA256 {
		return newError(CodeInvalid, "cell %s lineage does not bind the matrix layout revision", cell)
	}
	if lineage.SourceAssetID != m.SourceAsset.AssetID || lineage.SourceAssetSHA256 != m.SourceAsset.SHA256 {
		return newError(CodeInvalid, "cell %s lineage does not bind the matrix source asset", cell)
	}
	if !sameEvidence(lineage.EvidenceSHA256, m.EvidenceSHA256) {
		return newError(CodeInvalid, "cell %s lineage does not bind the matrix evidence", cell)
	}
	if lineage.Target != entry.Target || lineage.State != entry.State {
		return newError(CodeInvalid, "cell %s lineage claims %s/%s", cell, lineage.Target, lineage.State)
	}
	if lineage.TargetMatrix.ID != m.MatrixID {
		return newError(CodeInvalid, "cell %s lineage belongs to matrix %s", cell, lineage.TargetMatrix.ID)
	}
	if !sameStringSet(lineage.TargetMatrix.Targets, m.TargetMatrix.Targets) {
		return newError(CodeInvalid, "cell %s lineage target set does not match the matrix", cell)
	}
	return nil
}

func sameEvidence(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
