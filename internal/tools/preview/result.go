package previewtool

import (
	"encoding/json"

	"code-agent/internal/preview"
)

const resultSchema = "ui_preview_result.v1"

// result is the stable ui_preview_result.v1 side-channel. It never carries
// artifact identity: the preview runtime assigns artifact_id, revision, and
// viewer/asset bytes after the typed draft is produced.
type result struct {
	Schema        string              `json:"schema"`
	ArtifactKey   string              `json:"artifact_key"`
	Kind          string              `json:"kind"`
	Title         string              `json:"title"`
	Mode          string              `json:"mode"`
	Lineage       *preview.Lineage    `json:"lineage"`
	FallbackAsset resultFallbackAsset `json:"fallback_asset"`
}

type resultFallbackAsset struct {
	AssetID      string `json:"asset_id"`
	RelativePath string `json:"relative_path"`
	MIMEType     string `json:"mime_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	SHA256       string `json:"sha256"`
}

func buildResult(artifactKey, kind, title string, mode preview.PreviewMode, lineage *preview.Lineage, fallback resultFallbackAsset) result {
	return result{
		Schema:        resultSchema,
		ArtifactKey:   artifactKey,
		Kind:          kind,
		Title:         title,
		Mode:          string(mode),
		Lineage:       lineage,
		FallbackAsset: fallback,
	}
}

func (r result) marshal() (json.RawMessage, error) {
	return json.Marshal(r)
}
