package uilayout

import (
	"encoding/json"
	"math"
)

type normalizedBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type layoutCanvas struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type layoutSource struct {
	AssetID           string       `json:"asset_id"`
	SourceAssetSHA256 string       `json:"source_asset_sha256"`
	EvidenceSchema    string       `json:"evidence_schema"`
	EvidenceSHA256    string       `json:"evidence_sha256"`
	Canvas            layoutCanvas `json:"canvas"`
}

type layoutNode struct {
	ID          string         `json:"id"`
	BBox        *normalizedBox `json:"bbox"`
	Evidence    string         `json:"evidence"`
	EvidenceIDs []string       `json:"evidence_ids"`
	Children    []layoutNode   `json:"children"`
}

type layoutResponsive struct {
	Targets map[string]json.RawMessage `json:"targets"`
}

type layoutDocument struct {
	Schema      string            `json:"schema"`
	Source      layoutSource      `json:"source"`
	Root        layoutNode        `json:"root"`
	Responsive  layoutResponsive  `json:"responsive"`
	Assumptions []json.RawMessage `json:"assumptions"`
}

func decodeLayout(data []byte) (layoutDocument, error) {
	var document layoutDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return layoutDocument{}, err
	}
	return document, nil
}

func countLayoutNodes(node layoutNode) int {
	count := 1
	for _, child := range node.Children {
		count += countLayoutNodes(child)
	}
	return count
}

func (document layoutDocument) targets() []string {
	out := make([]string, 0, len(document.Responsive.Targets))
	for target := range document.Responsive.Targets {
		out = append(out, target)
	}
	return out
}

func (box normalizedBox) pixelRect(width, height int) (x, y, w, h int) {
	x = int(math.Round(box.X * float64(width)))
	y = int(math.Round(box.Y * float64(height)))
	w = int(math.Round(box.Width * float64(width)))
	h = int(math.Round(box.Height * float64(height)))
	return x, y, w, h
}
