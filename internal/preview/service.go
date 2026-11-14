package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	assets "code-agent/internal/assetref"
	"code-agent/internal/workspace"
)

// AssetLoader resolves bytes only after the producing tool has authorized an
// asset reference. Drafts never carry filesystem paths.
type AssetLoader interface {
	Load(ctx context.Context, workspaceRoot string, ref assets.Ref) ([]byte, string, error)
}

type WorkspaceAssetLoader struct{}

func (WorkspaceAssetLoader) Load(ctx context.Context, workspaceRoot string, ref assets.Ref) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil, "", NewError(CodeSecurityViolation, "Preview asset workspace is unavailable.", false)
	}
	var candidate string
	switch {
	case ref.WorkspaceRelativePath != "":
		if filepath.IsAbs(ref.WorkspaceRelativePath) {
			return nil, "", NewError(CodeSecurityViolation, "Preview asset path must be workspace-relative.", false)
		}
		rel := filepath.Clean(ref.WorkspaceRelativePath)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, "", NewError(CodeSecurityViolation, "Preview asset path escapes the workspace.", false)
		}
		candidate = filepath.Join(workspaceRoot, rel)
	case ref.AbsolutePath != "":
		candidate = filepath.Clean(ref.AbsolutePath)
	default:
		return nil, "", NewError(CodeAssetUnavailable, "Preview asset bytes are unavailable.", false)
	}
	if err := workspace.ValidatePath(workspaceRoot, candidate); err != nil {
		return nil, "", NewError(CodeSecurityViolation, "Preview asset path escapes the workspace.", false)
	}
	data, err := os.ReadFile(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", NewError(CodeAssetUnavailable, "Preview asset bytes are unavailable.", false)
		}
		return nil, "", NewError(CodeStoreUnavailable, "Preview asset bytes could not be read.", true)
	}
	return data, ref.MIMEType, nil
}

type Service struct {
	Store         Store
	AssetLoader   AssetLoader
	Now           func() time.Time
	MaxViewerSize int64

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewService(store Store) *Service {
	return &Service{Store: store, AssetLoader: WorkspaceAssetLoader{}, Now: time.Now, locks: make(map[string]*sync.Mutex)}
}

func (s *Service) Available() bool {
	return s != nil && s.Store != nil
}

func (s *Service) Produce(ctx context.Context, request ProduceRequest) ProduceResult {
	var result ProduceResult
	if !s.Available() {
		result.Fatal = NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
		return result
	}
	if len(request.Drafts) > MaxDraftsPerResult {
		for i := MaxDraftsPerResult; i < len(request.Drafts); i++ {
			result.Rejected = append(result.Rejected, DraftRejection{Index: i, Err: NewError(CodeInvalidArtifact, "A tool result may contain at most four preview drafts.", false)})
		}
		request.Drafts = request.Drafts[:MaxDraftsPerResult]
	}
	counts := make(map[string]int)
	for _, draft := range request.Drafts {
		if draft.ArtifactKey != "" {
			counts[draft.ArtifactKey]++
		}
	}
	for i, draft := range request.Drafts {
		if counts[draft.ArtifactKey] > 1 {
			result.Rejected = append(result.Rejected, DraftRejection{Index: i, Err: NewError(CodeInvalidArtifact, "Preview artifact keys must be unique within one tool result.", false)})
			continue
		}
		artifact, err := s.produceOne(ctx, request, draft)
		if err != nil {
			result.Rejected = append(result.Rejected, DraftRejection{Index: i, Err: err})
			continue
		}
		result.Artifacts = append(result.Artifacts, artifact)
	}
	return result
}

func (s *Service) produceOne(ctx context.Context, request ProduceRequest, draft Draft) (Artifact, *Error) {
	if strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.TurnID) == "" {
		return Artifact{}, NewError(CodeInvalidArtifact, "Preview correlation is incomplete.", false)
	}
	if strings.TrimSpace(request.ProducerID) == "" || strings.TrimSpace(request.Capability) == "" {
		return Artifact{}, NewError(CodeInvalidArtifact, "Preview producer identity is unavailable.", false)
	}
	if err := validateDraft(draft); err != nil {
		return Artifact{}, err
	}
	mode, modeErr := normalizeDraftMode(draft.Mode)
	if modeErr != nil {
		return Artifact{}, modeErr
	}
	if err := validateDraftLineage(mode, draft.Lineage); err != nil {
		return Artifact{}, err
	}
	if mode == ModeAdaptation {
		if err := ValidateLineageForPreview(ctx, request.WorkspaceRoot, draft.Lineage); err != nil {
			return Artifact{}, NewError(CodeInvalidArtifact, "Preview lineage does not match the reviewed layout revision: "+err.Error(), false)
		}
	}
	namespace := Namespace{
		SessionID: request.SessionID, Capability: request.Capability,
		ProducerID: request.ProducerID, ArtifactKey: draft.ArtifactKey,
	}
	lock := s.artifactLock(namespace)
	lock.Lock()
	defer lock.Unlock()
	latest, found, err := s.Store.Latest(ctx, namespace)
	if err != nil {
		return Artifact{}, asPreviewError(err, CodeStoreUnavailable, "Preview store is unavailable.")
	}
	now := s.now().UTC().Truncate(time.Second)
	revision := int64(1)
	createdAt := now
	if found {
		revision = latest.Revision + 1
		createdAt = latest.CreatedAt
	}
	artifact := Artifact{
		SchemaVersion: SchemaVersion,
		ArtifactID:    ArtifactID(namespace),
		Revision:      revision,
		Status:        draftStatus(draft),
		Kind:          draft.Kind,
		Title:         strings.TrimSpace(draft.Title),
		Summary:       strings.TrimSpace(draft.Summary),
		Capability:    request.Capability,
		ProducerID:    request.ProducerID,
		SessionID:     request.SessionID,
		TurnID:        request.TurnID,
		CallID:        request.CallID,
		CreatedAt:     createdAt,
		UpdatedAt:     now,
		ExpiresAt:     nil,
		Presentation:  presentationFor(draft.Presentation),
		Assets:        []Asset{},
		Fallback:      fallbackFor(draft.Fallback),
		Security:      DefaultSecurity(),
		Error:         nil,
		Metadata:      map[string]any{MetadataPreviewMode: string(mode)},
	}
	if draft.Lineage != nil {
		artifact.Metadata[MetadataLineage] = draft.Lineage
	}

	assetBytes := make(map[string][]byte, len(draft.Assets))
	for _, draftAsset := range draft.Assets {
		ref, ok := findAuthorizedAsset(request.Assets, draftAsset.AssetID)
		if !ok {
			return Artifact{}, NewError(CodeInvalidArtifact, "Preview asset is not authorized by the tool result.", false)
		}
		if request.ClientOrigin {
			if ref.AbsolutePath != "" || !clientSnapshotPath(ref.WorkspaceRelativePath) {
				return Artifact{}, NewError(CodeSecurityViolation, "Client preview assets must reference an authorized runtime snapshot.", false)
			}
		}
		data, sourceMIME, loadErr := s.assetLoader().Load(ctx, request.WorkspaceRoot, ref)
		if loadErr != nil {
			return Artifact{}, asPreviewError(loadErr, CodeAssetUnavailable, "Preview asset bytes are unavailable.")
		}
		if sourceMIME != "" && normalizedMIME(sourceMIME) != normalizedMIME(draftAsset.MIMEType) {
			return Artifact{}, NewError(CodeInvalidArtifact, "Preview asset MIME type does not match the authorized asset.", false)
		}
		if !allowedRasterMIME(draftAsset.MIMEType) {
			return Artifact{}, NewError(CodeInvalidArtifact, "Preview asset MIME type is not allowed.", false)
		}
		if err := validateRaster(data, draftAsset.MIMEType); err != nil {
			return Artifact{}, err
		}
		assetBytes[draftAsset.AssetID] = data
		artifact.Assets = append(artifact.Assets, Asset{
			AssetID:               draftAsset.AssetID,
			Role:                  draftAsset.Role,
			MIMEType:              normalizedMIME(draftAsset.MIMEType),
			SizeBytes:             int64(len(data)),
			SHA256:                DigestBytes(data),
			ContentURL:            AssetURL(request.SessionID, artifact.ArtifactID, revision, draftAsset.AssetID),
			Width:                 draftAsset.Width,
			Height:                draftAsset.Height,
			Alt:                   draftAsset.Alt,
			WorkspaceRelativePath: cleanDisplayPath(ref.WorkspaceRelativePath),
		})
	}
	var viewerBytes []byte
	if artifact.Kind == KindInteractiveHTML && artifact.Status == StatusReady {
		var materializeErr *Error
		viewerBytes, materializeErr = MaterializeViewer(artifact.Title, draft.ViewerHTML)
		if materializeErr != nil {
			return Artifact{}, materializeErr
		}
		artifact.Viewer = &Viewer{
			URL:       ViewerURL(request.SessionID, artifact.ArtifactID, revision),
			MIMEType:  ViewerMIMEType,
			SizeBytes: int64(len(viewerBytes)),
			SHA256:    DigestBytes(viewerBytes),
		}
	}
	hash, hashErr := RevisionSHA256(artifact)
	if hashErr != nil {
		return Artifact{}, NewError(CodeInvalidArtifact, "Preview revision digest could not be computed.", false)
	}
	artifact.RevisionSHA256 = hash
	if err := artifact.Validate(); err != nil {
		return Artifact{}, err
	}
	if putErr := s.Store.Put(ctx, namespace, artifact, viewerBytes, assetBytes); putErr != nil {
		previewErr := asPreviewError(putErr, CodeStoreUnavailable, "Preview store is unavailable.")
		if previewErr.Code == CodeRevisionConflict {
			return Artifact{}, previewErr
		}
		return failedSnapshot(artifact, previewErr), nil
	}
	return artifact, nil
}

func failedSnapshot(artifact Artifact, cause *Error) Artifact {
	artifact.Status = StatusFailed
	artifact.Viewer = nil
	artifact.Error = &Error{
		Code:      cause.Code,
		Message:   cause.Message,
		Retryable: true,
		HTTP:      cause.HTTP,
	}
	if artifact.Fallback == nil {
		artifact.Fallback = nil
	}
	hash, err := RevisionSHA256(artifact)
	if err == nil {
		artifact.RevisionSHA256 = hash
	}
	return artifact
}

func (s *Service) Resolve(sessionID, artifactID string, revision int64) (Resource, error) {
	return s.ResolveViewer(context.Background(), sessionID, artifactID, revision)
}

func (s *Service) ResolveViewer(ctx context.Context, sessionID, artifactID string, revision int64) (Resource, error) {
	artifact, err := s.load(ctx, sessionID, artifactID, revision)
	if err != nil {
		return Resource{}, err
	}
	if artifact.Status != StatusReady || artifact.Kind != KindInteractiveHTML || artifact.Viewer == nil {
		return Resource{}, NewError(CodeAssetUnavailable, "Preview viewer is unavailable.", false)
	}
	data, readErr := s.Store.ReadViewer(ctx, artifact)
	if readErr != nil {
		return Resource{}, readErr
	}
	return Resource{Data: data, Status: artifact.Status, MIMEType: artifact.Viewer.MIMEType, SizeBytes: int64(len(data)), SHA256: artifact.Viewer.SHA256, ExpiresAt: artifact.ExpiresAt}, nil
}

func (s *Service) ResolveContent(sessionID, artifactID string, revision int64, assetID string) (Resource, error) {
	return s.ResolveAsset(context.Background(), sessionID, artifactID, revision, assetID)
}

func (s *Service) ResolveAsset(ctx context.Context, sessionID, artifactID string, revision int64, assetID string) (Resource, error) {
	artifact, err := s.load(ctx, sessionID, artifactID, revision)
	if err != nil {
		return Resource{}, err
	}
	asset, ok := artifact.FindAsset(assetID)
	if !ok {
		return Resource{}, NewError(CodeAssetUnavailable, "Preview asset is unavailable.", false)
	}
	data, readErr := s.Store.ReadAsset(ctx, artifact, assetID)
	if readErr != nil {
		return Resource{}, readErr
	}
	return Resource{Data: data, Status: artifact.Status, MIMEType: asset.MIMEType, SizeBytes: int64(len(data)), SHA256: asset.SHA256, ExpiresAt: artifact.ExpiresAt}, nil
}

func (s *Service) load(ctx context.Context, sessionID, artifactID string, revision int64) (Artifact, error) {
	if !s.Available() {
		return Artifact{}, NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if !validatePublicID(sessionID) || !validatePublicID(artifactID) || revision <= 0 {
		return Artifact{}, NewError(CodeInvalidArtifact, "Preview artifact identity is invalid.", false)
	}
	artifact, err := s.Store.GetArtifact(ctx, sessionID, artifactID, revision)
	if err != nil {
		return Artifact{}, asPreviewError(err, CodeStoreUnavailable, "Preview store is unavailable.")
	}
	if artifact.Expired(s.now()) {
		return Artifact{}, NewError(CodeExpired, "Preview bytes have expired.", false)
	}
	return artifact, nil
}

// MaterializePrivateViewer resolves and writes verified viewer bytes to a
// private temporary directory for CLI/TUI handoff. Raw model fragments are never
// written or opened.
func (s *Service) MaterializePrivateViewer(ctx context.Context, dir, sessionID, artifactID string, revision int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	resource, err := s.ResolveViewer(ctx, sessionID, artifactID, revision)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create preview handoff directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("secure preview handoff directory: %w", err)
	}
	f, err := os.CreateTemp(dir, "viewer-*.html")
	if err != nil {
		return "", fmt.Errorf("create preview handoff file: %w", err)
	}
	name := f.Name()
	if err := f.Chmod(0o600); err == nil {
		_, err = f.Write(resource.Data)
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("write preview handoff file: %w", err)
	}
	if closeErr != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close preview handoff file: %w", closeErr)
	}
	return name, nil
}

func (s *Service) artifactLock(namespace Namespace) *sync.Mutex {
	key := strings.Join([]string{namespace.SessionID, namespace.Capability, namespace.ProducerID, namespace.ArtifactKey}, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	lock := s.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[key] = lock
	}
	return lock
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) assetLoader() AssetLoader {
	if s.AssetLoader != nil {
		return s.AssetLoader
	}
	return WorkspaceAssetLoader{}
}

func validateDraft(draft Draft) *Error {
	if strings.TrimSpace(draft.ArtifactKey) == "" || strings.TrimSpace(draft.Title) == "" {
		return NewError(CodeInvalidArtifact, "Preview artifact key and title are required.", false)
	}
	if draft.Kind != KindInteractiveHTML && draft.Kind != KindStaticImage {
		return NewError(CodeInvalidArtifact, "Preview kind is unsupported.", false)
	}
	seen := make(map[string]bool)
	for _, asset := range draft.Assets {
		if strings.TrimSpace(asset.AssetID) == "" || seen[asset.AssetID] {
			return NewError(CodeInvalidArtifact, "Preview asset IDs must be non-empty and unique.", false)
		}
		seen[asset.AssetID] = true
		if asset.Role != RolePrimary && asset.Role != RoleFallback {
			return NewError(CodeInvalidArtifact, "Preview asset role is unsupported.", false)
		}
		if !allowedRasterMIME(asset.MIMEType) {
			return NewError(CodeInvalidArtifact, "Preview asset MIME type is not allowed.", false)
		}
	}
	if draft.Fallback != nil {
		asset, ok := draftAsset(draft.Assets, draft.Fallback.AssetID)
		if !ok || asset.Role != RoleFallback {
			return NewError(CodeInvalidArtifact, "Preview fallback must reference a fallback asset.", false)
		}
	}
	switch draft.Kind {
	case KindInteractiveHTML:
		if strings.TrimSpace(draft.ViewerHTML) != "" {
			if draft.Fallback == nil {
				return NewError(CodeInvalidArtifact, "Ready interactive preview requires fallback.", false)
			}
			if len(draft.ViewerHTML) > MaxViewerBytes {
				return NewError(CodeViewerTooLarge, "Viewer exceeds the 2 MiB limit.", false)
			}
		}
	case KindStaticImage:
		if draft.Fallback != nil {
			return NewError(CodeInvalidArtifact, "Static preview must use primary assets for display.", false)
		}
		primary := false
		for _, asset := range draft.Assets {
			if asset.Role == RolePrimary {
				primary = true
			}
		}
		if !primary {
			return NewError(CodeInvalidArtifact, "Static preview requires a primary image asset.", false)
		}
	}
	return nil
}

func draftStatus(draft Draft) Status {
	if draft.Kind == KindStaticImage {
		return StatusReady
	}
	if strings.TrimSpace(draft.ViewerHTML) == "" {
		return StatusPending
	}
	return StatusReady
}

func presentationFor(hint *PresentationHint) Presentation {
	out := DefaultPresentation()
	if hint == nil {
		return out
	}
	if hint.Placement != "" {
		out.Placement = hint.Placement
	}
	if hint.PreferredAspectRatio > 0 {
		out.PreferredAspectRatio = hint.PreferredAspectRatio
	}
	if hint.MinHeight > 0 {
		out.MinHeight = hint.MinHeight
	}
	if hint.InitialState != "" {
		out.InitialState = hint.InitialState
	}
	return out
}

func fallbackFor(draft *FallbackDraft) *Fallback {
	if draft == nil {
		return nil
	}
	return &Fallback{AssetID: draft.AssetID, Text: draft.Text}
}

func draftAsset(assets []AssetDraft, id string) (AssetDraft, bool) {
	for _, asset := range assets {
		if asset.AssetID == id {
			return asset, true
		}
	}
	return AssetDraft{}, false
}

func findAuthorizedAsset(refs []assets.Ref, id string) (assets.Ref, bool) {
	for _, ref := range refs {
		if ref.ID == id {
			return ref, true
		}
	}
	return assets.Ref{}, false
}

func clientSnapshotPath(value string) bool {
	if value == "" || filepath.IsAbs(value) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	return strings.HasPrefix(clean, ".codeagent/assets/client/")
}

func cleanDisplayPath(value string) string {
	if value == "" || filepath.IsAbs(value) {
		return ""
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}

func normalizedMIME(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
}

func validateRaster(data []byte, mimeType string) *Error {
	if len(data) < 12 {
		return NewError(CodeInvalidArtifact, "Preview image is invalid.", false)
	}
	switch normalizedMIME(mimeType) {
	case "image/png":
		if !bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
			return NewError(CodeInvalidArtifact, "Preview PNG is invalid.", false)
		}
	case "image/jpeg":
		if data[0] != 0xff || data[1] != 0xd8 {
			return NewError(CodeInvalidArtifact, "Preview JPEG is invalid.", false)
		}
	case "image/webp":
		if string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return NewError(CodeInvalidArtifact, "Preview WebP is invalid.", false)
		}
	default:
		return NewError(CodeInvalidArtifact, "Preview image MIME type is not allowed.", false)
	}
	if normalizedMIME(mimeType) == "image/webp" {
		return nil
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		return NewError(CodeInvalidArtifact, "Preview image cannot be decoded.", false)
	}
	return nil
}

func asPreviewError(err error, fallbackCode ErrorCode, fallbackMessage string) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return NewError(fallbackCode, fallbackMessage, fallbackCode == CodeStoreUnavailable)
}

var _ PreviewViewerResolver = (*Service)(nil)
var _ PreviewAssetResolver = (*Service)(nil)
