package previewmatrix

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"code-agent/internal/preview"
	"code-agent/internal/preview/matrix"
)

const resultSchema = "ui_preview_matrix_result.v1"

// result is the stable, model-visible ui_preview_matrix_result.v1 projection.
// It carries workspace-relative asset paths only: the model can follow them
// with ordinary read tools, while internal absolute paths never leave the
// runtime.
type result struct {
	Schema         string          `json:"schema"`
	MatrixID       string          `json:"matrix_id"`
	MatrixRevision int64           `json:"matrix_revision"`
	MatrixSHA256   string          `json:"matrix_sha256"`
	Created        bool            `json:"created"`
	MatrixPath     string          `json:"matrix_path"`
	ManifestSchema string          `json:"manifest_schema"`
	ManifestSHA256 string          `json:"manifest_sha256"`
	ManifestPath   string          `json:"manifest_path"`
	SourceAsset    sourceAsset     `json:"source_asset"`
	Layout         layoutRef       `json:"layout"`
	EvidenceSHA256 *string         `json:"evidence_sha256"`
	TargetMatrix   targetMatrix    `json:"target_matrix"`
	States         []string        `json:"states"`
	Coverage       matrix.Coverage `json:"coverage"`
	Entries        []resultEntry   `json:"entries"`
	ErrorTypes     []string        `json:"error_types"`
}

type sourceAsset struct {
	AssetID string `json:"asset_id"`
	SHA256  string `json:"sha256"`
}

type layoutRef struct {
	ArtifactID     string `json:"artifact_id"`
	Revision       int64  `json:"revision"`
	RevisionSHA256 string `json:"revision_sha256"`
}

type targetMatrix struct {
	ID      string   `json:"id"`
	Targets []string `json:"targets"`
}

// resultPreviewRef is the resolved, runtime-owned identity of one preview
// revision. It is the reference the model can pass back to other tools.
type resultPreviewRef struct {
	ArtifactID     string `json:"artifact_id"`
	Revision       int64  `json:"revision"`
	RevisionSHA256 string `json:"revision_sha256"`
}

type resultEntry struct {
	Target        string                `json:"target"`
	State         string                `json:"state"`
	Status        matrix.EntryStatus    `json:"status"`
	PreviewRef    *resultPreviewRef     `json:"preview_ref"`
	PreviewMode   *string               `json:"preview_mode"`
	FallbackAsset *matrix.FallbackAsset `json:"fallback_asset"`
	Error         *matrix.EntryError    `json:"error"`
}

func buildResult(document matrix.Matrix, manifest matrix.Manifest, manifestBytes []byte, persisted matrix.Persisted) result {
	entries := make([]resultEntry, 0, len(document.Entries))
	errorTypes := make(map[string]struct{})
	for _, entry := range document.Entries {
		out := resultEntry{
			Target:        entry.Target,
			State:         entry.State,
			Status:        entry.Status,
			PreviewMode:   entry.PreviewMode,
			FallbackAsset: entry.FallbackAsset,
			Error:         entry.Error,
		}
		if entry.PreviewArtifactID != nil && entry.PreviewRevision != nil && entry.PreviewRevisionSHA256 != nil {
			out.PreviewRef = &resultPreviewRef{
				ArtifactID:     *entry.PreviewArtifactID,
				Revision:       *entry.PreviewRevision,
				RevisionSHA256: *entry.PreviewRevisionSHA256,
			}
		}
		if entry.Error != nil && entry.Error.Code != "" {
			errorTypes[entry.Error.Code] = struct{}{}
		}
		entries = append(entries, out)
	}
	codes := make([]string, 0, len(errorTypes))
	for code := range errorTypes {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return result{
		Schema:         resultSchema,
		MatrixID:       document.MatrixID,
		MatrixRevision: persisted.Revision,
		MatrixSHA256:   document.MatrixSHA256,
		Created:        persisted.Created,
		MatrixPath:     persisted.MatrixPath,
		ManifestSchema: manifest.Schema,
		ManifestSHA256: preview.DigestBytes(manifestBytes),
		ManifestPath:   persisted.ManifestPath,
		SourceAsset: sourceAsset{
			AssetID: document.SourceAsset.AssetID,
			SHA256:  document.SourceAsset.SHA256,
		},
		Layout: layoutRef{
			ArtifactID:     document.Layout.ArtifactID,
			Revision:       document.Layout.Revision,
			RevisionSHA256: document.Layout.RevisionSHA256,
		},
		EvidenceSHA256: document.EvidenceSHA256,
		TargetMatrix: targetMatrix{
			ID:      document.TargetMatrix.ID,
			Targets: append([]string(nil), document.TargetMatrix.Targets...),
		},
		States:     append([]string(nil), document.States...),
		Coverage:   document.Coverage,
		Entries:    entries,
		ErrorTypes: codes,
	}
}

func (r result) marshal() (json.RawMessage, error) {
	return json.Marshal(r)
}

// modelContent renders the model-visible text channel. ToolResult.Output is the
// stable structured contract; the agent loop only commits Content into the model
// transcript, so the resolved references, paths, digests, and per-cell outcomes
// must also appear here. Paths stay workspace-relative.
func modelContent(verb string, out result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s ui_preview_matrix %s revision %d: %d/%d completed, %d failed, %d missing.\n",
		verb, out.MatrixID, out.MatrixRevision,
		out.Coverage.Completed, out.Coverage.Expected, out.Coverage.Failed, out.Coverage.Missing)
	fmt.Fprintf(&b, "matrix_sha256=%s matrix_path=%s manifest_path=%s\n",
		out.MatrixSHA256, out.MatrixPath, out.ManifestPath)
	fmt.Fprintf(&b, "layout=%s revision=%d revision_sha256=%s source_asset=%s\n",
		out.Layout.ArtifactID, out.Layout.Revision, out.Layout.RevisionSHA256, out.SourceAsset.AssetID)
	for _, entry := range out.Entries {
		fmt.Fprintf(&b, "%s/%s %s", entry.Target, entry.State, entry.Status)
		if entry.PreviewRef != nil {
			fmt.Fprintf(&b, " preview_id=%s revision=%d preview_sha256=%s",
				entry.PreviewRef.ArtifactID, entry.PreviewRef.Revision, entry.PreviewRef.RevisionSHA256)
		}
		if entry.PreviewMode != nil {
			fmt.Fprintf(&b, " mode=%s", *entry.PreviewMode)
		}
		if entry.FallbackAsset != nil {
			fmt.Fprintf(&b, " fallback_asset=%s fallback_sha256=%s",
				entry.FallbackAsset.AssetID, entry.FallbackAsset.SHA256)
		}
		if entry.Error != nil {
			fmt.Fprintf(&b, " error=%s message=%q", entry.Error.Code, entry.Error.Message)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
