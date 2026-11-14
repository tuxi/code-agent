package preview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	assets "code-agent/internal/assetref"
)

const (
	SchemaVersion      = "preview.artifact.v1"
	FixtureVersion     = "preview.fixtures.v1"
	ContractID         = "preview_contract_v1"
	SecurityProfile    = "preview.security.v1"
	ViewerMIMEType     = "text/html; charset=utf-8"
	MaxViewerBytes     = 2 * 1024 * 1024
	MaxDraftsPerResult = 4
	MaxErrorTextBytes  = 1024
)

type Status string

const (
	StatusPending     Status = "pending"
	StatusReady       Status = "ready"
	StatusFailed      Status = "failed"
	StatusUnavailable Status = "unavailable"
)

type Kind string

const (
	KindInteractiveHTML Kind = "interactive_html"
	KindStaticImage     Kind = "static_image"
)

type AssetRole string

const (
	RolePrimary  AssetRole = "primary"
	RoleFallback AssetRole = "fallback"
)

type ErrorCode string

const (
	CodeUnsupported        ErrorCode = "preview_unsupported"
	CodeArtifactMissing    ErrorCode = "preview_artifact_missing"
	CodeInvalidArtifact    ErrorCode = "preview_invalid_artifact"
	CodeViewerTooLarge     ErrorCode = "preview_viewer_too_large"
	CodeViewerMIMEMismatch ErrorCode = "preview_viewer_mime_mismatch"
	CodeDigestMismatch     ErrorCode = "preview_digest_mismatch"
	CodeViewerFetchFailed  ErrorCode = "preview_viewer_fetch_failed"
	CodeSecurityViolation  ErrorCode = "preview_security_violation"
	CodeRenderFailed       ErrorCode = "preview_render_failed"
	CodeAssetUnavailable   ErrorCode = "preview_asset_unavailable"
	CodeExpired            ErrorCode = "preview_expired"
	CodeRevisionConflict   ErrorCode = "preview_revision_conflict"
	CodeStoreUnavailable   ErrorCode = "preview_store_unavailable"
)

// Error is the runtime artifact/API error schema. It is data, not a turn
// failure, and the HTTP status is transport metadata rather than JSON.
type Error struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	HTTP      int       `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Code)
}

func NewError(code ErrorCode, message string, retryable bool) *Error {
	status, ok := HTTPStatusForCode(code)
	if !ok {
		status = http.StatusInternalServerError
	}
	return &Error{Code: code, Message: message, Retryable: retryable, HTTP: status}
}

func HTTPStatusForCode(code ErrorCode) (int, bool) {
	switch code {
	case CodeUnsupported:
		return http.StatusOK, true
	case CodeArtifactMissing, CodeAssetUnavailable:
		return http.StatusNotFound, true
	case CodeInvalidArtifact, CodeDigestMismatch:
		return http.StatusUnprocessableEntity, true
	case CodeViewerTooLarge:
		return http.StatusRequestEntityTooLarge, true
	case CodeViewerMIMEMismatch:
		return http.StatusUnsupportedMediaType, true
	case CodeViewerFetchFailed:
		return http.StatusBadGateway, true
	case CodeSecurityViolation:
		return http.StatusForbidden, true
	case CodeExpired:
		return http.StatusGone, true
	case CodeRevisionConflict:
		return http.StatusConflict, true
	case CodeStoreUnavailable:
		return http.StatusServiceUnavailable, true
	default:
		return 0, false
	}
}

type Presentation struct {
	Placement            string  `json:"placement"`
	PreferredAspectRatio float64 `json:"preferred_aspect_ratio,omitempty"`
	MinHeight            int     `json:"min_height,omitempty"`
	InitialState         string  `json:"initial_state"`
}

type Viewer struct {
	URL       string `json:"url"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type Asset struct {
	AssetID               string    `json:"asset_id"`
	Role                  AssetRole `json:"role"`
	MIMEType              string    `json:"mime_type"`
	SizeBytes             int64     `json:"size_bytes"`
	SHA256                string    `json:"sha256"`
	ContentURL            string    `json:"content_url"`
	Width                 int       `json:"width,omitempty"`
	Height                int       `json:"height,omitempty"`
	Alt                   string    `json:"alt,omitempty"`
	WorkspaceRelativePath string    `json:"workspace_relative_path,omitempty"`
}

type Fallback struct {
	AssetID string `json:"asset_id"`
	Text    string `json:"text"`
}

type Security struct {
	Profile        string `json:"profile"`
	JavaScript     string `json:"javascript"`
	Network        string `json:"network"`
	Navigation     string `json:"navigation"`
	PersistentData string `json:"persistent_data"`
	Actions        string `json:"actions"`
}

type Artifact struct {
	SchemaVersion  string         `json:"schema_version"`
	ArtifactID     string         `json:"artifact_id"`
	Revision       int64          `json:"revision"`
	RevisionSHA256 string         `json:"revision_sha256,omitempty"`
	Status         Status         `json:"status"`
	Kind           Kind           `json:"kind"`
	Title          string         `json:"title"`
	Summary        string         `json:"summary"`
	Capability     string         `json:"capability"`
	ProducerID     string         `json:"producer_id"`
	SessionID      string         `json:"session_id"`
	TurnID         string         `json:"turn_id"`
	CallID         string         `json:"call_id"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	ExpiresAt      *time.Time     `json:"expires_at"`
	Presentation   Presentation   `json:"presentation"`
	Viewer         *Viewer        `json:"viewer,omitempty"`
	Assets         []Asset        `json:"assets"`
	Fallback       *Fallback      `json:"fallback"`
	Security       Security       `json:"security"`
	Error          *Error         `json:"error"`
	Metadata       map[string]any `json:"metadata"`
}

type PreviewArtifact = Artifact

type PresentationHint struct {
	Placement            string  `json:"placement,omitempty"`
	PreferredAspectRatio float64 `json:"preferred_aspect_ratio,omitempty"`
	MinHeight            int     `json:"min_height,omitempty"`
	InitialState         string  `json:"initial_state,omitempty"`
}

type AssetDraft struct {
	AssetID  string    `json:"asset_id"`
	Role     AssetRole `json:"role"`
	MIMEType string    `json:"mime_type"`
	Alt      string    `json:"alt,omitempty"`
	Width    int       `json:"width,omitempty"`
	Height   int       `json:"height,omitempty"`
}

func (a *AssetDraft) UnmarshalJSON(data []byte) error {
	type alias AssetDraft
	var out alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return NewError(CodeInvalidArtifact, "Preview asset draft contains an unsupported field.", false)
	}
	*a = AssetDraft(out)
	return nil
}

type FallbackDraft struct {
	AssetID string `json:"asset_id"`
	Text    string `json:"text"`
}

func (f *FallbackDraft) UnmarshalJSON(data []byte) error {
	type alias FallbackDraft
	var out alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return NewError(CodeInvalidArtifact, "Preview fallback draft contains an unsupported field.", false)
	}
	*f = FallbackDraft(out)
	return nil
}

type Draft struct {
	ArtifactKey  string            `json:"artifact_key"`
	Mode         PreviewMode       `json:"mode,omitempty"`
	Lineage      *Lineage          `json:"lineage,omitempty"`
	Kind         Kind              `json:"kind"`
	Title        string            `json:"title"`
	Summary      string            `json:"summary,omitempty"`
	ViewerHTML   string            `json:"viewer_html,omitempty"`
	Assets       []AssetDraft      `json:"assets,omitempty"`
	Fallback     *FallbackDraft    `json:"fallback,omitempty"`
	Presentation *PresentationHint `json:"presentation,omitempty"`
}

func (d *Draft) UnmarshalJSON(data []byte) error {
	type alias Draft
	var out alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return NewError(CodeInvalidArtifact, "Preview draft contains an unsupported field.", false)
	}
	*d = Draft(out)
	return nil
}

// Names used by the frozen logical producer contract.
type PreviewDraft = Draft
type PreviewAssetDraft = AssetDraft
type PreviewFallbackDraft = FallbackDraft
type PreviewPresentationHint = PresentationHint

type ProduceRequest struct {
	SessionID     string
	TurnID        string
	CallID        string
	WorkspaceRoot string
	ProducerID    string
	Capability    string
	Assets        []assets.Ref
	ClientOrigin  bool
	Drafts        []Draft
}

type DraftRejection struct {
	Index int
	Err   *Error
}

type ProduceResult struct {
	Artifacts []Artifact
	Rejected  []DraftRejection
	Fatal     *Error
}

type Resource struct {
	Data      []byte
	Status    Status
	MIMEType  string
	SizeBytes int64
	SHA256    string
	ExpiresAt *time.Time
}

// PreviewViewerResolver is the frozen transport-neutral viewer resolver.
type PreviewViewerResolver interface {
	Resolve(sessionID, artifactID string, revision int64) (Resource, error)
}

// PreviewAssetResolver is the frozen transport-neutral asset resolver.
type PreviewAssetResolver interface {
	ResolveContent(sessionID, artifactID string, revision int64, assetID string) (Resource, error)
}

// Compatibility aliases keep call sites concise without changing the frozen
// semantic interface names.
type ViewerResolver = PreviewViewerResolver
type AssetResolver = PreviewAssetResolver

func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func DigestMatches(want string, data []byte) bool {
	return strings.EqualFold(strings.TrimSpace(want), DigestBytes(data))
}

func DefaultPresentation() Presentation {
	return Presentation{Placement: "conversation_inline", InitialState: "collapsed"}
}

func DefaultSecurity() Security {
	return Security{
		Profile:        SecurityProfile,
		JavaScript:     "isolated_only",
		Network:        "deny",
		Navigation:     "deny",
		PersistentData: "deny",
		Actions:        "none",
	}
}

func ViewerURL(sessionID, artifactID string, revision int64) string {
	return fmt.Sprintf("/v1/conversations/%s/previews/%s/revisions/%d/viewer", pathSegment(sessionID), pathSegment(artifactID), revision)
}

func AssetURL(sessionID, artifactID string, revision int64, assetID string) string {
	return fmt.Sprintf("/v1/conversations/%s/previews/%s/revisions/%d/assets/%s/content", pathSegment(sessionID), pathSegment(artifactID), revision, pathSegment(assetID))
}

func pathSegment(value string) string { return strings.TrimSpace(value) }

var publicIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,191}$`)

func validatePublicID(value string) bool {
	return publicIDPattern.MatchString(value)
}

func (a Artifact) FindAsset(assetID string) (Asset, bool) {
	for _, asset := range a.Assets {
		if asset.AssetID == assetID {
			return asset, true
		}
	}
	return Asset{}, false
}

func (a Artifact) Expired(now time.Time) bool {
	return a.ExpiresAt != nil && !now.Before(*a.ExpiresAt)
}

func (a Artifact) Clone() Artifact {
	out := a
	if a.Assets != nil {
		out.Assets = append([]Asset{}, a.Assets...)
	}
	if a.Viewer != nil {
		v := *a.Viewer
		out.Viewer = &v
	}
	if a.Fallback != nil {
		f := *a.Fallback
		out.Fallback = &f
	}
	if a.Error != nil {
		e := *a.Error
		out.Error = &e
	}
	if a.Metadata != nil {
		out.Metadata = make(map[string]any, len(a.Metadata))
		for k, v := range a.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

func (a Artifact) Validate() *Error {
	if a.SchemaVersion != SchemaVersion {
		return NewError(CodeInvalidArtifact, "Unknown preview artifact schema version.", false)
	}
	if !validatePublicID(a.ArtifactID) || a.Revision <= 0 {
		return NewError(CodeInvalidArtifact, "Preview artifact identity is invalid.", false)
	}
	if !validatePublicID(a.SessionID) || strings.TrimSpace(a.TurnID) == "" {
		return NewError(CodeInvalidArtifact, "Preview artifact correlation is incomplete.", false)
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		return NewError(CodeInvalidArtifact, "Preview artifact timestamps are invalid.", false)
	}
	if strings.TrimSpace(a.Title) == "" {
		return NewError(CodeInvalidArtifact, "Preview title is required.", false)
	}
	if a.Presentation.Placement != "conversation_inline" {
		return NewError(CodeInvalidArtifact, "Preview placement is invalid.", false)
	}
	if a.Presentation.InitialState != "collapsed" && a.Presentation.InitialState != "expanded" {
		return NewError(CodeInvalidArtifact, "Preview initial state is invalid.", false)
	}
	if a.Security != DefaultSecurity() {
		return NewError(CodeInvalidArtifact, "Preview security metadata is invalid.", false)
	}
	if err := validateArtifactLineage(a.Metadata); err != nil {
		return err
	}
	if a.Error != nil {
		if len([]byte(a.Error.Message)) > MaxErrorTextBytes {
			return NewError(CodeInvalidArtifact, "Preview error message is too large.", false)
		}
		if !artifactErrorCodeAllowed(a.Error.Code) {
			return NewError(CodeInvalidArtifact, "Preview artifact error code is invalid.", false)
		}
	}
	for _, asset := range a.Assets {
		if err := validateAsset(asset); err != nil {
			return err
		}
	}
	switch a.Status {
	case StatusPending:
		if a.Kind != KindInteractiveHTML && a.Kind != KindStaticImage {
			return NewError(CodeInvalidArtifact, "Preview kind is unsupported.", false)
		}
		if a.Error != nil {
			return NewError(CodeInvalidArtifact, "Pending preview must not carry an error.", false)
		}
	case StatusReady:
		if a.Error != nil {
			return NewError(CodeInvalidArtifact, "Ready preview must not carry an error.", false)
		}
		switch a.Kind {
		case KindInteractiveHTML:
			if a.Viewer == nil || a.Fallback == nil {
				return NewError(CodeInvalidArtifact, "Ready interactive preview requires viewer and fallback.", false)
			}
			if a.Viewer.MIMEType != ViewerMIMEType || a.Viewer.SizeBytes < 0 || !validDigest(a.Viewer.SHA256) || a.Viewer.SizeBytes > MaxViewerBytes {
				return NewError(CodeInvalidArtifact, "Preview viewer metadata is invalid.", false)
			}
			fallback, ok := a.FindAsset(a.Fallback.AssetID)
			if !ok || fallback.Role != RoleFallback {
				return NewError(CodeInvalidArtifact, "Ready interactive preview requires a fallback asset.", false)
			}
		case KindStaticImage:
			primary := 0
			for _, asset := range a.Assets {
				if asset.Role == RolePrimary && allowedRasterMIME(asset.MIMEType) {
					primary++
				}
			}
			if primary == 0 {
				return NewError(CodeInvalidArtifact, "Ready static preview requires a primary image asset.", false)
			}
		default:
			return NewError(CodeInvalidArtifact, "Preview kind is unsupported.", false)
		}
	case StatusFailed, StatusUnavailable:
		if a.Error == nil {
			return NewError(CodeInvalidArtifact, "Failed or unavailable preview requires an error.", false)
		}
		if a.Fallback != nil {
			fallback, ok := a.FindAsset(a.Fallback.AssetID)
			if !ok || fallback.Role != RoleFallback {
				return NewError(CodeInvalidArtifact, "Preview fallback asset is missing.", false)
			}
		}
	default:
		return NewError(CodeInvalidArtifact, "Preview status is unsupported.", false)
	}
	if a.Fallback != nil {
		if _, ok := a.FindAsset(a.Fallback.AssetID); !ok {
			return NewError(CodeInvalidArtifact, "Preview fallback asset is missing.", false)
		}
	}
	want, err := RevisionSHA256(a)
	if err != nil {
		return NewError(CodeInvalidArtifact, "Preview revision digest could not be computed.", false)
	}
	if a.RevisionSHA256 == "" || !strings.EqualFold(a.RevisionSHA256, want) {
		return NewError(CodeInvalidArtifact, "Preview revision digest is invalid.", false)
	}
	return nil
}

func validateAsset(asset Asset) *Error {
	if !validatePublicID(asset.AssetID) || (asset.Role != RolePrimary && asset.Role != RoleFallback) {
		return NewError(CodeInvalidArtifact, "Preview asset identity is invalid.", false)
	}
	if !allowedRasterMIME(asset.MIMEType) || asset.SizeBytes < 0 || !validDigest(asset.SHA256) {
		return NewError(CodeInvalidArtifact, "Preview asset metadata is invalid.", false)
	}
	if asset.ContentURL == "" || !strings.HasPrefix(asset.ContentURL, "/v1/conversations/") {
		return NewError(CodeInvalidArtifact, "Preview asset URL is invalid.", false)
	}
	if asset.Width < 0 || asset.Height < 0 {
		return NewError(CodeInvalidArtifact, "Preview asset dimensions are invalid.", false)
	}
	return nil
}

func artifactErrorCodeAllowed(code ErrorCode) bool {
	switch code {
	case CodeUnsupported, CodeArtifactMissing, CodeInvalidArtifact, CodeViewerTooLarge,
		CodeViewerMIMEMismatch, CodeDigestMismatch, CodeViewerFetchFailed,
		CodeSecurityViolation, CodeAssetUnavailable, CodeExpired,
		CodeRevisionConflict, CodeStoreUnavailable:
		return true
	default:
		return false
	}
}

func allowedRasterMIME(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0])) {
	case "image/png", "image/jpeg", "image/webp":
		return true
	default:
		return false
	}
}

func validDigest(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func IsPreviewError(err error) bool {
	var target *Error
	return errors.As(err, &target)
}
