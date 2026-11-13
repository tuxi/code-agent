package uilayout

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"code-agent/internal/workspace"
)

const revisionDirPrefix = "revision-"

var artifactIDPattern = regexp.MustCompile(`^layout_[A-Za-z0-9._-]{1,180}$`)

type revisionMetadata struct {
	Kind                 string       `json:"kind"`
	LayoutArtifactID     string       `json:"layout_artifact_id"`
	LayoutRevision       int          `json:"layout_revision"`
	LayoutRevisionSHA256 string       `json:"layout_revision_sha256"`
	LayoutSHA256         string       `json:"layout_sha256"`
	LayoutPath           string       `json:"layout_path"`
	OverlayPath          string       `json:"overlay_path"`
	OverlayWidth         int          `json:"overlay_width"`
	OverlayHeight        int          `json:"overlay_height"`
	SourcePath           string       `json:"source_path"`
	SourceAssetID        string       `json:"source_asset_id"`
	SourceAssetSHA256    string       `json:"source_asset_sha256"`
	SourceMIMEType       string       `json:"source_mime_type"`
	Canvas               layoutCanvas `json:"canvas"`
	EvidencePath         string       `json:"evidence_path,omitempty"`
	EvidenceSHA256       string       `json:"evidence_sha256,omitempty"`
	NodeCount            int          `json:"node_count"`
	AssumptionCount      int          `json:"assumption_count"`
	CreatedAt            string       `json:"created_at"`
}

type persistedRevision struct {
	Revision    int
	LayoutPath  string
	OverlayPath string
	Created     bool
}

// persistRevision stores one content-addressed layout revision. An identical
// layout for the same source reuses its existing revision.
func persistRevision(root, artifactID string, layoutCanonical, overlay []byte, meta revisionMetadata) (persistedRevision, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return persistedRevision{}, err
	}
	base := filepath.Join(rootAbs, ".codeagent", "assets", "ui-layout")
	if workspace.ClassifyPath(rootAbs, base) != workspace.PathInsideWorkspace {
		return persistedRevision{}, fmt.Errorf("ui_layout asset directory escapes the workspace")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return persistedRevision{}, err
	}
	baseCanonical, err := workspace.CanonicalPath(base)
	if err != nil {
		return persistedRevision{}, err
	}
	rootCanonical, err := workspace.CanonicalPath(rootAbs)
	if err != nil {
		return persistedRevision{}, err
	}
	if workspace.ClassifyPath(rootCanonical, baseCanonical) != workspace.PathInsideWorkspace {
		return persistedRevision{}, fmt.Errorf("ui_layout asset directory escapes the workspace through a symlink")
	}
	artifactDir := filepath.Join(baseCanonical, artifactID)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return persistedRevision{}, err
	}
	artifactDir, err = workspace.CanonicalPath(artifactDir)
	if err != nil {
		return persistedRevision{}, err
	}
	if workspace.ClassifyPath(rootCanonical, artifactDir) != workspace.PathInsideWorkspace {
		return persistedRevision{}, fmt.Errorf("ui_layout artifact directory escapes the workspace")
	}
	if rel, relErr := filepath.Rel(baseCanonical, artifactDir); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return persistedRevision{}, fmt.Errorf("ui_layout artifact directory escapes the asset root")
	}

	latest, existing, err := findExistingRevision(artifactDir, meta)
	if err != nil {
		return persistedRevision{}, err
	}
	if existing != nil {
		return *existing, nil
	}

	for revision := latest + 1; ; revision++ {
		revisionDir := filepath.Join(artifactDir, revisionName(revision))
		if err := os.Mkdir(revisionDir, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return persistedRevision{}, err
		}
		layoutRel, err := filepath.Rel(rootCanonical, filepath.Join(revisionDir, "layout.json"))
		if err != nil {
			return persistedRevision{}, err
		}
		overlayRel, err := filepath.Rel(rootCanonical, filepath.Join(revisionDir, "overlay.png"))
		if err != nil {
			return persistedRevision{}, err
		}
		meta.LayoutRevision = revision
		meta.LayoutPath = filepath.ToSlash(layoutRel)
		meta.OverlayPath = filepath.ToSlash(overlayRel)
		meta.CreatedAt = time.Now().UTC().Format(time.RFC3339)

		metadataBytes, err := json.Marshal(meta)
		if err != nil {
			return persistedRevision{}, err
		}
		if err := writeFileAtomic(revisionDir, "layout.json", layoutCanonical); err != nil {
			return persistedRevision{}, err
		}
		if err := writeFileAtomic(revisionDir, "overlay.png", overlay); err != nil {
			return persistedRevision{}, err
		}
		if err := writeFileAtomic(revisionDir, "metadata.json", metadataBytes); err != nil {
			return persistedRevision{}, err
		}
		return persistedRevision{
			Revision:    revision,
			LayoutPath:  meta.LayoutPath,
			OverlayPath: meta.OverlayPath,
			Created:     true,
		}, nil
	}
}

func findExistingRevision(artifactDir string, meta revisionMetadata) (int, *persistedRevision, error) {
	entries, err := os.ReadDir(artifactDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	type revisionEntry struct {
		revision int
		name     string
	}
	revisions := make([]revisionEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), revisionDirPrefix) {
			continue
		}
		revision, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), revisionDirPrefix))
		if err != nil || revision < 1 {
			continue
		}
		revisions = append(revisions, revisionEntry{revision: revision, name: entry.Name()})
	}
	sort.Slice(revisions, func(i, j int) bool { return revisions[i].revision < revisions[j].revision })
	latest := 0
	for _, entry := range revisions {
		if entry.revision > latest {
			latest = entry.revision
		}
		data, err := os.ReadFile(filepath.Join(artifactDir, entry.name, "metadata.json"))
		if err != nil {
			continue
		}
		var existing revisionMetadata
		if err := json.Unmarshal(data, &existing); err != nil {
			continue
		}
		if existing.LayoutSHA256 == meta.LayoutSHA256 &&
			existing.SourceAssetSHA256 == meta.SourceAssetSHA256 &&
			existing.EvidenceSHA256 == meta.EvidenceSHA256 &&
			existing.OverlayWidth == meta.OverlayWidth &&
			existing.OverlayHeight == meta.OverlayHeight &&
			existing.Canvas == meta.Canvas &&
			revisionFilesExist(artifactDir, entry.name) {
			return latest, &persistedRevision{
				Revision:    existing.LayoutRevision,
				LayoutPath:  existing.LayoutPath,
				OverlayPath: existing.OverlayPath,
				Created:     false,
			}, nil
		}
	}
	return latest, nil, nil
}

func revisionFilesExist(artifactDir, revisionDirName string) bool {
	for _, name := range []string{"layout.json", "overlay.png"} {
		info, err := os.Stat(filepath.Join(artifactDir, revisionDirName, name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func revisionName(revision int) string {
	return revisionDirPrefix + fmt.Sprintf("%06d", revision)
}

func writeFileAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, err
	}
	return json.Marshal(value)
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
