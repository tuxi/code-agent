package matrix

import (
	"encoding/json"
	"fmt"
)

// ManifestEntry is the replay-facing projection of one cell.
type ManifestEntry struct {
	Target                string      `json:"target"`
	State                 string      `json:"state"`
	Status                EntryStatus `json:"status"`
	PreviewArtifactID     *string     `json:"preview_artifact_id"`
	PreviewRevision       *int64      `json:"preview_revision"`
	PreviewRevisionSHA256 *string     `json:"preview_revision_sha256"`
	Error                 *EntryError `json:"error"`
}

// Manifest is the stable, time-free summary of a matrix. It carries no
// filesystem paths so two builds of the same inputs produce identical bytes.
type Manifest struct {
	Schema         string          `json:"schema"`
	MatrixID       string          `json:"matrix_id"`
	MatrixSHA256   string          `json:"matrix_sha256"`
	SourceAsset    SourceIdentity  `json:"source_asset"`
	Layout         LayoutIdentity  `json:"layout"`
	EvidenceSHA256 *string         `json:"evidence_sha256"`
	TargetMatrix   TargetMatrix    `json:"target_matrix"`
	States         []string        `json:"states"`
	Coverage       Coverage        `json:"coverage"`
	Entries        []ManifestEntry `json:"entries"`
}

// BuildManifest derives the manifest from a matrix. The matrix must already be
// sealed; Validate is the caller's job when the matrix came from outside.
func BuildManifest(m Matrix) Manifest {
	entries := make([]ManifestEntry, 0, len(m.Entries))
	for _, entry := range m.Entries {
		entries = append(entries, ManifestEntry{
			Target:                entry.Target,
			State:                 entry.State,
			Status:                entry.Status,
			PreviewArtifactID:     entry.PreviewArtifactID,
			PreviewRevision:       entry.PreviewRevision,
			PreviewRevisionSHA256: entry.PreviewRevisionSHA256,
			Error:                 entry.Error,
		})
	}
	return Manifest{
		Schema:         ManifestSchema,
		MatrixID:       m.MatrixID,
		MatrixSHA256:   m.MatrixSHA256,
		SourceAsset:    m.SourceAsset,
		Layout:         m.Layout,
		EvidenceSHA256: m.EvidenceSHA256,
		TargetMatrix:   copyTargetMatrix(m.TargetMatrix),
		States:         append([]string(nil), m.States...),
		Coverage:       m.Coverage,
		Entries:        entries,
	}
}

// Marshal renders the manifest with stable formatting.
func (m Manifest) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal matrix manifest: %w", err)
	}
	return append(data, '\n'), nil
}

func copyTargetMatrix(in TargetMatrix) TargetMatrix {
	return TargetMatrix{ID: in.ID, Targets: append([]string(nil), in.Targets...)}
}
