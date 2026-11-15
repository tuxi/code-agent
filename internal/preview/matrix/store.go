package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"code-agent/internal/workspace"
)

const revisionMetadataKind = "ui_preview_matrix_revision"

// Persisted is the workspace location of one stored matrix revision.
type Persisted struct {
	Revision     int64
	MatrixPath   string
	ManifestPath string
	Created      bool
}

// Result is one build outcome: the sealed document, its stable manifest, and
// where it was persisted (zero when it was not).
type Result struct {
	Matrix    Matrix
	Manifest  Manifest
	Persisted Persisted
}

// BuildAndPersist builds the matrix and stores it under the workspace asset
// root, reusing an identical existing revision instead of writing a new one.
func (b *Builder) BuildAndPersist(ctx context.Context, request Request) (Result, error) {
	document, err := b.Build(ctx, request)
	if err != nil {
		return Result{}, err
	}
	persisted, err := Persist(ctx, request.WorkspaceRoot, document)
	if err != nil {
		return Result{}, err
	}
	return Result{Matrix: document, Manifest: BuildManifest(document), Persisted: persisted}, nil
}

// Marshal renders the matrix document with stable formatting.
func Marshal(m Matrix) ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, newError(CodeInvalid, "matrix could not be encoded: %v", err)
	}
	return append(data, '\n'), nil
}

// Decode validates and decodes one matrix document.
func Decode(data []byte) (Matrix, error) {
	if err := ValidateDocument(data); err != nil {
		return Matrix{}, err
	}
	var document Matrix
	if err := json.Unmarshal(data, &document); err != nil {
		return Matrix{}, newError(CodeInvalid, "matrix is invalid: %v", err)
	}
	return document, nil
}

type revisionMetadata struct {
	Kind           string   `json:"kind"`
	MatrixID       string   `json:"matrix_id"`
	MatrixRevision int64    `json:"matrix_revision"`
	MatrixSHA256   string   `json:"matrix_sha256"`
	Coverage       Coverage `json:"coverage"`
	CreatedAt      string   `json:"created_at"`
}

// Persist writes the matrix and its manifest under
// .codeagent/assets/ui-preview-matrix/<matrix_id>/revision-<n>/. An identical
// matrix that is already stored is reused, so replaying a build is idempotent.
func Persist(ctx context.Context, workspaceRoot string, m Matrix) (Persisted, error) {
	if err := ctx.Err(); err != nil {
		return Persisted{}, err
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return Persisted{}, newError(CodeInvalid, "matrix persistence requires a workspace root")
	}
	if err := m.Validate(); err != nil {
		return Persisted{}, err
	}
	rootAbs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return Persisted{}, newError(CodeInvalid, "matrix workspace could not be resolved")
	}
	rootCanonical, err := workspace.CanonicalPath(rootAbs)
	if err != nil {
		return Persisted{}, newError(CodeStoreUnavailable, "matrix workspace could not be canonicalized")
	}
	base := filepath.Join(rootCanonical, filepath.FromSlash(Directory))
	if workspace.ClassifyPath(rootCanonical, base) != workspace.PathInsideWorkspace {
		return Persisted{}, newError(CodeInvalid, "matrix asset directory escapes the workspace")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return Persisted{}, newError(CodeStoreUnavailable, "matrix asset directory is unavailable: %v", err)
	}
	baseCanonical, err := workspace.CanonicalPath(base)
	if err != nil {
		return Persisted{}, newError(CodeStoreUnavailable, "matrix asset directory could not be canonicalized")
	}
	if workspace.ClassifyPath(rootCanonical, baseCanonical) != workspace.PathInsideWorkspace {
		return Persisted{}, newError(CodeInvalid, "matrix asset directory escapes the workspace through a symlink")
	}
	matrixDir := filepath.Join(baseCanonical, m.MatrixID)
	if err := os.MkdirAll(matrixDir, 0o700); err != nil {
		return Persisted{}, newError(CodeStoreUnavailable, "matrix directory is unavailable: %v", err)
	}
	matrixDir, err = workspace.CanonicalPath(matrixDir)
	if err != nil {
		return Persisted{}, newError(CodeStoreUnavailable, "matrix directory could not be canonicalized")
	}
	if workspace.ClassifyPath(rootCanonical, matrixDir) != workspace.PathInsideWorkspace {
		return Persisted{}, newError(CodeInvalid, "matrix directory escapes the workspace")
	}

	matrixBytes, err := Marshal(m)
	if err != nil {
		return Persisted{}, err
	}
	manifestBytes, err := BuildManifest(m).Marshal()
	if err != nil {
		return Persisted{}, newError(CodeStoreUnavailable, "%v", err)
	}

	latest, existing, err := findStoredRevision(rootCanonical, matrixDir, m, matrixBytes, manifestBytes)
	if err != nil {
		return Persisted{}, err
	}
	if existing != nil {
		return *existing, nil
	}

	for revision := latest + 1; ; revision++ {
		dir := filepath.Join(matrixDir, fmt.Sprintf(matrixRevisionFmt, revision))
		if err := os.Mkdir(dir, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return Persisted{}, newError(CodeStoreUnavailable, "matrix revision directory is unavailable: %v", err)
		}
		metadata := revisionMetadata{
			Kind:           revisionMetadataKind,
			MatrixID:       m.MatrixID,
			MatrixRevision: revision,
			MatrixSHA256:   m.MatrixSHA256,
			Coverage:       m.Coverage,
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		}
		metadataBytes, err := json.Marshal(metadata)
		if err != nil {
			_ = os.RemoveAll(dir)
			return Persisted{}, newError(CodeStoreUnavailable, "matrix metadata could not be encoded: %v", err)
		}
		for name, data := range map[string][]byte{
			"matrix.json":   matrixBytes,
			"manifest.json": manifestBytes,
			"metadata.json": metadataBytes,
		} {
			if err := writeFileAtomic(dir, name, data); err != nil {
				_ = os.RemoveAll(dir)
				return Persisted{}, newError(CodeStoreUnavailable, "matrix revision could not be written: %v", err)
			}
		}
		matrixPath, ok := workspaceRelativePath(rootCanonical, filepath.Join(dir, "matrix.json"))
		if !ok {
			_ = os.RemoveAll(dir)
			return Persisted{}, newError(CodeStoreUnavailable, "matrix path could not be made workspace-relative")
		}
		manifestPath, ok := workspaceRelativePath(rootCanonical, filepath.Join(dir, "manifest.json"))
		if !ok {
			_ = os.RemoveAll(dir)
			return Persisted{}, newError(CodeStoreUnavailable, "manifest path could not be made workspace-relative")
		}
		return Persisted{Revision: revision, MatrixPath: matrixPath, ManifestPath: manifestPath, Created: true}, nil
	}
}

func findStoredRevision(rootCanonical, matrixDir string, m Matrix, matrixBytes, manifestBytes []byte) (int64, *Persisted, error) {
	entries, err := os.ReadDir(matrixDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, newError(CodeStoreUnavailable, "matrix directory is unavailable: %v", err)
	}
	type candidate struct {
		revision int64
		name     string
	}
	var revisions []candidate
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "revision-") {
			continue
		}
		revision, parseErr := strconv.ParseInt(strings.TrimPrefix(entry.Name(), "revision-"), 10, 64)
		if parseErr != nil || revision < 1 {
			continue
		}
		revisions = append(revisions, candidate{revision: revision, name: entry.Name()})
	}
	sort.Slice(revisions, func(i, j int) bool { return revisions[i].revision < revisions[j].revision })
	var latest int64
	var reused *Persisted
	for _, item := range revisions {
		if item.revision > latest {
			latest = item.revision
		}
		if reused != nil {
			continue
		}
		dir := filepath.Join(matrixDir, item.name)
		metadataBytes, err := readRegularFile(dir, "metadata.json")
		if err != nil {
			continue
		}
		var metadata revisionMetadata
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			continue
		}
		if metadata.Kind != revisionMetadataKind || metadata.MatrixID != m.MatrixID || metadata.MatrixSHA256 != m.MatrixSHA256 {
			continue
		}
		storedMatrix, err := readRegularFile(dir, "matrix.json")
		if err != nil || !bytes.Equal(storedMatrix, matrixBytes) {
			continue
		}
		storedManifest, err := readRegularFile(dir, "manifest.json")
		if err != nil || !bytes.Equal(storedManifest, manifestBytes) {
			continue
		}
		matrixPath, ok := workspaceRelativePath(rootCanonical, filepath.Join(dir, "matrix.json"))
		if !ok {
			continue
		}
		manifestPath, ok := workspaceRelativePath(rootCanonical, filepath.Join(dir, "manifest.json"))
		if !ok {
			continue
		}
		reused = &Persisted{Revision: item.revision, MatrixPath: matrixPath, ManifestPath: manifestPath, Created: false}
	}
	return latest, reused, nil
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

func workspaceRelativePath(rootCanonical, absolute string) (string, bool) {
	rel, err := filepath.Rel(rootCanonical, absolute)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}
