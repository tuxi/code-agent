// Package matrix aggregates independent ui_preview revisions into one
// verifiable ui_preview_matrix.v1 document and persists that document with a
// stable manifest. It reads the P1-F contracts without changing them: preview
// artifacts keep their frozen revision digest, and a completed cell must carry
// the exact ui_preview_lineage.v2 record the runtime validated.
package matrix

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"code-agent/internal/preview"
)

const (
	// Schema is the matrix document contract.
	Schema = "ui_preview_matrix.v1"
	// ManifestSchema identifies the stable manifest projection of a matrix.
	ManifestSchema = "ui_preview_matrix_manifest.v1"
	// MaxEntries bounds one matrix; it matches the schema.
	MaxEntries = 512
	// MaxErrorTextBytes bounds one entry error message.
	MaxErrorTextBytes = 1024

	// Directory is the workspace-relative root of persisted matrices.
	Directory = ".codeagent/assets/ui-preview-matrix"
)

// Build error codes. They never reach the matrix document: a matrix either
// records per-cell failures or is not produced at all.
const (
	CodeInvalid           = "matrix_invalid"
	CodeLayoutUnavailable = "matrix_layout_unavailable"
	CodeLayoutInvalid     = "matrix_layout_invalid"
	CodeLayoutMismatch    = "matrix_layout_mismatch"
	CodeStoreUnavailable  = "matrix_store_unavailable"
)

// PublicLayoutUnavailableMessage is the only model-visible text for a layout
// unavailable failure. Filesystem detail stays in Error.Diagnostic so tool
// results never carry absolute paths.
const PublicLayoutUnavailableMessage = "layout revision metadata is unavailable"

// Error is a typed build or persistence failure. Message is always safe to
// surface to a model; Diagnostic keeps runtime-only detail such as the failing
// filesystem path and must never be copied into a tool result.
type Error struct {
	Code       string
	Message    string
	diagnostic string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Diagnostic returns runtime-only failure detail. It may contain absolute
// paths and is intended for logs and tests, never for model-visible output.
func (e *Error) Diagnostic() string {
	if e == nil {
		return ""
	}
	return e.diagnostic
}

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func newErrorWithDiagnostic(code, message string, cause error) *Error {
	e := &Error{Code: code, Message: message}
	if cause != nil {
		e.diagnostic = cause.Error()
	}
	return e
}

// Diagnostic returns the runtime-only detail attached to a matrix error, or ""
// when err carries none.
func Diagnostic(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.diagnostic
	}
	return ""
}

// ErrorCode classifies err as a matrix error, or returns "matrix_invalid".
func ErrorCode(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return CodeInvalid
}

// IsMatrixError reports whether err is a typed matrix error.
func IsMatrixError(err error) bool {
	_, ok := err.(*Error)
	return ok
}

// EntryStatus is the per-cell outcome. Every expected cell carries exactly one.
type EntryStatus string

const (
	StatusCompleted EntryStatus = "completed"
	StatusFailed    EntryStatus = "failed"
	StatusMissing   EntryStatus = "missing"
)

// SourceIdentity is the source image the layout revision was derived from.
type SourceIdentity struct {
	AssetID string `json:"asset_id"`
	SHA256  string `json:"sha256"`
}

// LayoutIdentity pins the reviewed layout revision.
type LayoutIdentity struct {
	ArtifactID     string `json:"artifact_id"`
	Revision       int64  `json:"revision"`
	RevisionSHA256 string `json:"revision_sha256"`
}

// TargetMatrix is the target set shared by every entry lineage.
type TargetMatrix struct {
	ID      string   `json:"id"`
	Targets []string `json:"targets"`
}

// FallbackAsset is the static evidence a completed cell can fall back to.
type FallbackAsset struct {
	AssetID  string `json:"asset_id"`
	MIMEType string `json:"mime_type"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	SHA256   string `json:"sha256"`
}

// EntryError explains one failed cell. It never substitutes for a fallback or a
// viewer.
type EntryError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Entry is one target/state cell.
type Entry struct {
	Target                string           `json:"target"`
	State                 string           `json:"state"`
	Status                EntryStatus      `json:"status"`
	PreviewArtifactID     *string          `json:"preview_artifact_id"`
	PreviewRevision       *int64           `json:"preview_revision"`
	PreviewRevisionSHA256 *string          `json:"preview_revision_sha256"`
	PreviewMode           *string          `json:"preview_mode"`
	Lineage               *preview.Lineage `json:"lineage"`
	FallbackAsset         *FallbackAsset   `json:"fallback_asset"`
	Error                 *EntryError      `json:"error"`
}

// Coverage is the declared cell census. It must equal the actual entries.
type Coverage struct {
	Targets   int  `json:"targets"`
	States    int  `json:"states"`
	Expected  int  `json:"expected"`
	Completed int  `json:"completed"`
	Failed    int  `json:"failed"`
	Missing   int  `json:"missing"`
	Complete  bool `json:"complete"`
}

// Matrix is the executable ui_preview_matrix.v1 document.
type Matrix struct {
	Schema         string         `json:"schema"`
	MatrixID       string         `json:"matrix_id"`
	SourceAsset    SourceIdentity `json:"source_asset"`
	Layout         LayoutIdentity `json:"layout"`
	EvidenceSHA256 *string        `json:"evidence_sha256"`
	TargetMatrix   TargetMatrix   `json:"target_matrix"`
	States         []string       `json:"states"`
	Entries        []Entry        `json:"entries"`
	Coverage       Coverage       `json:"coverage"`
	MatrixSHA256   string         `json:"matrix_sha256"`
}

// Targets returns the matrix target set.
func (m Matrix) Targets() []string { return m.TargetMatrix.Targets }

// Digest computes the matrix digest over the RFC 8785 canonical JSON of the
// document with matrix_sha256 removed.
func (m Matrix) Digest() (string, error) {
	sealed := m
	sealed.MatrixSHA256 = ""
	canonical, err := preview.Canonicalize(sealed)
	if err != nil {
		return "", newError(CodeInvalid, "matrix digest could not be computed: %v", err)
	}
	return preview.DigestBytes(canonical), nil
}

// Seal computes and stores the matrix digest.
func (m *Matrix) Seal() error {
	digest, err := m.Digest()
	if err != nil {
		return err
	}
	m.MatrixSHA256 = digest
	return nil
}

// CanonicalJSON returns the stable JSON form used for replay comparison.
func (m Matrix) CanonicalJSON() ([]byte, error) {
	canonical, err := preview.Canonicalize(m)
	if err != nil {
		return nil, newError(CodeInvalid, "matrix could not be canonicalized: %v", err)
	}
	return canonical, nil
}

// Validate re-validates the document through the schema and the cross-field
// rules, so a persisted or replayed matrix cannot drift.
func (m Matrix) Validate() error {
	data, err := json.Marshal(m)
	if err != nil {
		return newError(CodeInvalid, "matrix could not be encoded: %v", err)
	}
	return ValidateDocument(data)
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func isRevisionSHA256(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	return isLowerHexSHA256(strings.TrimPrefix(value, "sha256:"))
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, value := range a {
		seen[value] = struct{}{}
	}
	for _, value := range b {
		if _, ok := seen[value]; !ok {
			return false
		}
	}
	return true
}
