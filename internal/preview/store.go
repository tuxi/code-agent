package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Namespace is the runtime-owned preview identity. ArtifactKey is intentionally
// absent from the wire; the store uses it only to allocate a stable artifact ID
// inside the runtime-derived session/capability/producer namespace.
type Namespace struct {
	SessionID   string
	Capability  string
	ProducerID  string
	ArtifactKey string
}

type Store interface {
	Latest(ctx context.Context, namespace Namespace) (Artifact, bool, error)
	GetArtifact(ctx context.Context, sessionID, artifactID string, revision int64) (Artifact, error)
	Put(ctx context.Context, namespace Namespace, artifact Artifact, viewer []byte, assets map[string][]byte) error
	ReadViewer(ctx context.Context, artifact Artifact) ([]byte, error)
	ReadAsset(ctx context.Context, artifact Artifact, assetID string) ([]byte, error)
}

type FSStore struct {
	base string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewFSStore(base string) (*FSStore, error) {
	if strings.TrimSpace(base) == "" {
		return nil, errors.New("preview store base directory is required")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("create preview store: %w", err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return nil, fmt.Errorf("secure preview store: %w", err)
	}
	probe, err := os.CreateTemp(base, ".write-probe-")
	if err != nil {
		return nil, fmt.Errorf("preview store is not writable: %w", err)
	}
	probeName := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(probeName)
	if closeErr != nil {
		return nil, fmt.Errorf("close preview store probe: %w", closeErr)
	}
	if removeErr != nil {
		return nil, fmt.Errorf("remove preview store probe: %w", removeErr)
	}
	return &FSStore{base: filepath.Clean(base), locks: make(map[string]*sync.Mutex)}, nil
}

func (s *FSStore) Latest(ctx context.Context, namespace Namespace) (Artifact, bool, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, false, err
	}
	if !validatePublicID(namespace.SessionID) {
		return Artifact{}, false, NewError(CodeInvalidArtifact, "Preview session identity is invalid.", false)
	}
	id := ArtifactID(namespace)
	revisions, err := s.revisions(namespace.SessionID, id)
	if err != nil {
		return Artifact{}, false, err
	}
	if len(revisions) == 0 {
		return Artifact{}, false, nil
	}
	artifact, err := s.GetArtifact(ctx, namespace.SessionID, id, revisions[len(revisions)-1])
	if err != nil {
		return Artifact{}, false, err
	}
	return artifact, true, nil
}

func (s *FSStore) GetArtifact(ctx context.Context, sessionID, artifactID string, revision int64) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if !validatePublicID(sessionID) || !validatePublicID(artifactID) || revision <= 0 {
		return Artifact{}, NewError(CodeInvalidArtifact, "Preview artifact identity is invalid.", false)
	}
	path := filepath.Join(s.revisionDir(sessionID, artifactID, revision), "artifact.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Artifact{}, NewError(CodeArtifactMissing, "Preview artifact was not found.", false)
		}
		return Artifact{}, NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return Artifact{}, NewError(CodeInvalidArtifact, "Preview artifact bytes are invalid.", false)
	}
	if artifact.SessionID != sessionID || artifact.ArtifactID != artifactID || artifact.Revision != revision {
		return Artifact{}, NewError(CodeInvalidArtifact, "Preview artifact binding is invalid.", false)
	}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func (s *FSStore) Put(ctx context.Context, namespace Namespace, artifact Artifact, viewer []byte, assets map[string][]byte) error {
	return s.put(ctx, namespace, artifact, viewer, assets, true)
}

func (s *FSStore) put(ctx context.Context, namespace Namespace, artifact Artifact, viewer []byte, assets map[string][]byte, enforceNamespace bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if (enforceNamespace && artifact.ArtifactID != ArtifactID(namespace)) || artifact.SessionID != namespace.SessionID {
		return NewError(CodeInvalidArtifact, "Preview artifact namespace is invalid.", false)
	}
	if err := artifact.Validate(); err != nil {
		return err
	}
	if artifact.Viewer != nil {
		if err := ValidateViewerBytes(viewer, artifact.Viewer.SizeBytes, artifact.Viewer.SHA256); err != nil {
			return err
		}
	} else if len(viewer) != 0 {
		return NewError(CodeInvalidArtifact, "Preview viewer bytes do not match the artifact.", false)
	}
	if len(artifact.Assets) > 0 && assets == nil {
		return NewError(CodeInvalidArtifact, "Preview asset bytes are missing.", false)
	}
	for _, asset := range artifact.Assets {
		data, ok := assets[asset.AssetID]
		if !ok || int64(len(data)) != asset.SizeBytes || !DigestMatches(asset.SHA256, data) {
			return NewError(CodeInvalidArtifact, "Preview asset bytes are missing or invalid.", false)
		}
	}

	lock := s.lockFor(namespace.SessionID + "\x00" + artifact.ArtifactID)
	lock.Lock()
	defer lock.Unlock()

	revisionDir := s.revisionDir(namespace.SessionID, artifact.ArtifactID, artifact.Revision)
	if existing, err := readArtifactFile(filepath.Join(revisionDir, "artifact.json")); err == nil {
		if strings.EqualFold(existing.RevisionSHA256, artifact.RevisionSHA256) {
			return nil
		}
		return NewError(CodeRevisionConflict, "Preview revision already exists with different bytes.", false)
	} else if !os.IsNotExist(err) {
		return NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}

	artifactDir := filepath.Dir(revisionDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	tmp, err := os.MkdirTemp(artifactDir, ".revision-")
	if err != nil {
		return NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmp)
		}
	}()
	artifactBytes, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return NewError(CodeInvalidArtifact, "Preview artifact could not be encoded.", false)
	}
	artifactBytes = append(artifactBytes, '\n')
	if err := writePrivate(filepath.Join(tmp, "artifact.json"), artifactBytes); err != nil {
		return NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	if len(viewer) > 0 {
		if err := writePrivate(filepath.Join(tmp, "viewer.html"), viewer); err != nil {
			return NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
		}
	}
	for _, asset := range artifact.Assets {
		if err := writePrivate(filepath.Join(tmp, assetFilename(asset.AssetID)), assets[asset.AssetID]); err != nil {
			return NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
		}
	}
	if err := os.Rename(tmp, revisionDir); err != nil {
		if existing, readErr := readArtifactFile(filepath.Join(revisionDir, "artifact.json")); readErr == nil {
			if strings.EqualFold(existing.RevisionSHA256, artifact.RevisionSHA256) {
				return nil
			}
			return NewError(CodeRevisionConflict, "Preview revision already exists with different bytes.", false)
		}
		return NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	committed = true
	return nil
}

func (s *FSStore) ReadViewer(ctx context.Context, artifact Artifact) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if artifact.Viewer == nil {
		return nil, NewError(CodeAssetUnavailable, "Preview viewer is unavailable.", false)
	}
	path := filepath.Join(s.revisionDir(artifact.SessionID, artifact.ArtifactID, artifact.Revision), "viewer.html")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NewError(CodeAssetUnavailable, "Preview viewer is unavailable.", false)
		}
		return nil, NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	if err := ValidateViewerBytes(data, artifact.Viewer.SizeBytes, artifact.Viewer.SHA256); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *FSStore) ReadAsset(ctx context.Context, artifact Artifact, assetID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	asset, ok := artifact.FindAsset(assetID)
	if !ok {
		return nil, NewError(CodeAssetUnavailable, "Preview asset is unavailable.", false)
	}
	path := filepath.Join(s.revisionDir(artifact.SessionID, artifact.ArtifactID, artifact.Revision), assetFilename(asset.AssetID))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NewError(CodeAssetUnavailable, "Preview asset is unavailable.", false)
		}
		return nil, NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	if int64(len(data)) != asset.SizeBytes || !DigestMatches(asset.SHA256, data) {
		return nil, NewError(CodeDigestMismatch, "Preview asset digest did not match.", false)
	}
	return data, nil
}

func (s *FSStore) revisions(sessionID, artifactID string) ([]int64, error) {
	artifactDir := filepath.Join(s.base, sessionDir(sessionID), artifactID)
	revs, err := os.ReadDir(artifactDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, NewError(CodeStoreUnavailable, "Preview store is unavailable.", true)
	}
	var ids []int64
	for _, rev := range revs {
		if !rev.IsDir() || strings.HasPrefix(rev.Name(), ".") {
			continue
		}
		n, parseErr := strconv.ParseInt(rev.Name(), 10, 64)
		if parseErr == nil && n > 0 {
			ids = append(ids, n)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (s *FSStore) revisionDir(sessionID, artifactID string, revision int64) string {
	return filepath.Join(s.base, sessionDir(sessionID), artifactID, strconv.FormatInt(revision, 10))
}

func (s *FSStore) lockFor(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock := s.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[key] = lock
	}
	return lock
}

func ArtifactID(namespace Namespace) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		namespace.SessionID, namespace.Capability, namespace.ProducerID, namespace.ArtifactKey,
	}, "\x00")))
	return "preview_" + hex.EncodeToString(sum[:])[:24]
}

func sessionDir(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return "session_" + hex.EncodeToString(sum[:])[:24]
}

func assetFilename(assetID string) string {
	sum := sha256.Sum256([]byte(assetID))
	return "asset_" + hex.EncodeToString(sum[:])[:24] + ".bin"
}

func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(f, bytes.NewReader(data)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readArtifactFile(path string) (Artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, err
	}
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

// ImportArtifact writes a fixture or replay snapshot using its existing
// revision and digest. It performs the same immutability checks as Put.
func (s *FSStore) ImportArtifact(ctx context.Context, namespace Namespace, artifact Artifact, viewer []byte, assets map[string][]byte) error {
	return s.put(ctx, namespace, artifact, viewer, assets, false)
}

// Ensure FSStore satisfies the persistence boundary.
var _ Store = (*FSStore)(nil)

// TimeNow is injectable in tests and services.
type Clock func() time.Time

func SystemClock() time.Time { return time.Now().UTC() }
