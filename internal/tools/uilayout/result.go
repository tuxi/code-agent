package uilayout

import (
	"encoding/json"
	"path"
	"sort"
)

const resultSchema = "ui_layout_result.v1"

// result is the stable ui_layout_result.v1 side-channel document. Raw paths
// live only on the two asset descriptors so every consumer resolves one
// canonical asset instead of re-deriving a path.
type result struct {
	Schema               string            `json:"schema"`
	LayoutArtifactID     string            `json:"layout_artifact_id"`
	LayoutRevision       int               `json:"layout_revision"`
	LayoutRevisionSHA256 string            `json:"layout_revision_sha256"`
	SourceAssetID        string            `json:"source_asset_id"`
	SourceAssetSHA256    string            `json:"source_asset_sha256"`
	EvidenceSHA256       *string           `json:"evidence_sha256"`
	Canvas               layoutCanvas      `json:"canvas"`
	NodeCount            int               `json:"node_count"`
	AssumptionCount      int               `json:"assumption_count"`
	Targets              []string          `json:"targets"`
	Created              bool              `json:"created"`
	OverlayAsset         layoutResultAsset `json:"overlay_asset"`
	LayoutJSONAsset      layoutResultAsset `json:"layout_json_asset"`
}

type layoutResultAsset struct {
	AssetID      string `json:"asset_id"`
	AssetRole    string `json:"asset_role"`
	Kind         string `json:"kind"`
	RelativePath string `json:"relative_path"`
	Filename     string `json:"filename"`
	MIMEType     string `json:"mime_type"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	SHA256       string `json:"sha256"`
}

type resultInputs struct {
	LayoutArtifactID     string
	LayoutRevision       int
	LayoutRevisionSHA256 string
	SourceAssetID        string
	SourceAssetSHA256    string
	EvidenceSHA256       string
	Canvas               layoutCanvas
	NodeCount            int
	AssumptionCount      int
	Targets              []string
	Created              bool
	OverlayAsset         layoutResultAsset
	LayoutJSONAsset      layoutResultAsset
}

func buildResult(in resultInputs) result {
	targets := append([]string(nil), in.Targets...)
	sort.Strings(targets)
	var evidence *string
	if in.EvidenceSHA256 != "" {
		value := in.EvidenceSHA256
		evidence = &value
	}
	return result{
		Schema:               resultSchema,
		LayoutArtifactID:     in.LayoutArtifactID,
		LayoutRevision:       in.LayoutRevision,
		LayoutRevisionSHA256: in.LayoutRevisionSHA256,
		SourceAssetID:        in.SourceAssetID,
		SourceAssetSHA256:    in.SourceAssetSHA256,
		EvidenceSHA256:       evidence,
		Canvas:               in.Canvas,
		NodeCount:            in.NodeCount,
		AssumptionCount:      in.AssumptionCount,
		Targets:              targets,
		Created:              in.Created,
		OverlayAsset:         in.OverlayAsset,
		LayoutJSONAsset:      in.LayoutJSONAsset,
	}
}

func makeResultAsset(assetID, role, kind, relativePath, mimeType, sha256 string, width, height int) layoutResultAsset {
	return layoutResultAsset{
		AssetID:      assetID,
		AssetRole:    role,
		Kind:         kind,
		RelativePath: relativePath,
		Filename:     path.Base(relativePath),
		MIMEType:     mimeType,
		Width:        width,
		Height:       height,
		SHA256:       sha256,
	}
}

func (r result) marshal() (json.RawMessage, error) {
	return json.Marshal(r)
}
