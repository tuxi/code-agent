// Package uicontract validates the executable JSON contracts used by visual
// layout understanding. It checks JSON Schema shape plus cross-reference rules
// that JSON Schema cannot express directly.
package uicontract

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/jsonschema-go/jsonschema"
)

// ValidateAgainstSchema validates one JSON document against a local JSON Schema.
func ValidateAgainstSchema(schemaData, documentData []byte) error {
	var document any
	if err := json.Unmarshal(documentData, &document); err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("resolve schema: %w", err)
	}
	if err := resolved.Validate(document); err != nil {
		return fmt.Errorf("schema validation: %w", err)
	}
	return nil
}

// ValidateUIEvidence validates structural invariants for ui_evidence.v1.
func ValidateUIEvidence(documentData []byte) error {
	var document evidenceDocument
	if err := decodeJSON(documentData, &document); err != nil {
		return err
	}

	elementIDs := make(map[string]struct{}, len(document.Elements))
	for i, element := range document.Elements {
		if err := addUniqueID(elementIDs, element.ID, fmt.Sprintf("elements[%d].id", i)); err != nil {
			return err
		}
		if err := validateBBox(fmt.Sprintf("elements[%d].bbox", i), element.BBox); err != nil {
			return err
		}
	}
	for i, element := range document.Elements {
		if element.ParentID != nil && *element.ParentID != "" {
			if _, ok := elementIDs[*element.ParentID]; !ok {
				return fmt.Errorf("elements[%d].parent_id %q does not reference an element", i, *element.ParentID)
			}
		}
	}

	repeatedIDs := make(map[string]struct{}, len(document.RepeatedGroups))
	for i, group := range document.RepeatedGroups {
		if err := addUniqueID(repeatedIDs, group.ID, fmt.Sprintf("repeated_groups[%d].id", i)); err != nil {
			return err
		}
		for j, id := range group.MemberIDs {
			if _, ok := elementIDs[id]; !ok {
				return fmt.Errorf("repeated_groups[%d].member_ids[%d] %q does not reference an element", i, j, id)
			}
		}
		if err := validateBBox(fmt.Sprintf("repeated_groups[%d].bbox", i), group.BBox); err != nil {
			return err
		}
	}

	treeIDs := make(map[string]struct{}, len(document.SourceTree.Nodes))
	for i, node := range document.SourceTree.Nodes {
		if err := addUniqueID(treeIDs, node.ID, fmt.Sprintf("source_tree.nodes[%d].id", i)); err != nil {
			return err
		}
		if node.BBox != nil {
			if err := validateBBox(fmt.Sprintf("source_tree.nodes[%d].bbox", i), *node.BBox); err != nil {
				return err
			}
		}
	}
	for i, node := range document.SourceTree.Nodes {
		if node.ParentID != nil && *node.ParentID != "" {
			if _, ok := treeIDs[*node.ParentID]; !ok {
				return fmt.Errorf("source_tree.nodes[%d].parent_id %q does not reference a source-tree node", i, *node.ParentID)
			}
		}
	}
	if document.SourceTree.Kind == "none" && len(document.SourceTree.Nodes) != 0 {
		return fmt.Errorf("source_tree.kind none requires an empty node list")
	}
	if document.SourceTree.Kind != "none" && len(document.SourceTree.Nodes) == 0 {
		return fmt.Errorf("source_tree.kind %q requires at least one node", document.SourceTree.Kind)
	}

	cropIDs := make(map[string]struct{}, len(document.Crops))
	for i, crop := range document.Crops {
		if err := addUniqueID(cropIDs, crop.AssetID, fmt.Sprintf("crops[%d].asset_id", i)); err != nil {
			return err
		}
		if err := validateBBox(fmt.Sprintf("crops[%d].source_bbox", i), crop.SourceBBox); err != nil {
			return err
		}
	}

	for i, uncertainty := range document.Uncertainties {
		for j, id := range uncertainty.RelatedElementIDs {
			if _, ok := elementIDs[id]; !ok {
				return fmt.Errorf("uncertainties[%d].related_element_ids[%d] %q does not reference an element", i, j, id)
			}
		}
	}

	if document.Limits.ElementCount != len(document.Elements) {
		return fmt.Errorf("limits.element_count %d does not match elements length %d", document.Limits.ElementCount, len(document.Elements))
	}
	return nil
}

// ValidateUILayout validates ui_layout.v1 semantics. When evidenceData is
// present, every evidence reference and the source identity must match it.
func ValidateUILayout(documentData, evidenceData []byte) error {
	var document layoutDocument
	if err := decodeJSON(documentData, &document); err != nil {
		return err
	}

	nodeIDs := make(map[string]struct{})
	nodeCount := 0
	if err := walkLayoutNode(document.Root, nodeIDs, &nodeCount); err != nil {
		return err
	}
	if nodeCount > 1000 {
		return fmt.Errorf("layout has %d nodes, maximum is 1000", nodeCount)
	}

	invariantIDs := make(map[string]struct{}, len(document.Responsive.Invariants))
	for i, invariant := range document.Responsive.Invariants {
		if err := addUniqueID(invariantIDs, invariant.ID, fmt.Sprintf("responsive.invariants[%d].id", i)); err != nil {
			return err
		}
	}

	policyNodes := make(map[string]struct{}, len(document.Responsive.NodePolicies))
	for i, policy := range document.Responsive.NodePolicies {
		if _, ok := nodeIDs[policy.NodeID]; !ok {
			return fmt.Errorf("responsive.node_policies[%d].node_id %q does not reference a layout node", i, policy.NodeID)
		}
		if _, exists := policyNodes[policy.NodeID]; exists {
			return fmt.Errorf("responsive.node_policies contains duplicate policy for node %q", policy.NodeID)
		}
		policyNodes[policy.NodeID] = struct{}{}
	}

	var evidenceIDs map[string]struct{}
	if len(evidenceData) > 0 {
		var evidence evidenceDocument
		if err := decodeJSON(evidenceData, &evidence); err != nil {
			return fmt.Errorf("decode evidence: %w", err)
		}
		evidenceIDs = make(map[string]struct{}, len(evidence.Elements))
		for _, element := range evidence.Elements {
			evidenceIDs[element.ID] = struct{}{}
		}
		if document.Source.AssetID != evidence.Source.AssetID {
			return fmt.Errorf("source.asset_id %q does not match evidence asset %q", document.Source.AssetID, evidence.Source.AssetID)
		}
		if document.Source.SourceAssetSHA256 != evidence.Source.SHA256 {
			return fmt.Errorf("source.source_asset_sha256 does not match evidence source sha256")
		}
	}

	if evidenceIDs != nil {
		if err := checkLayoutEvidenceReferences(document.Root, evidenceIDs); err != nil {
			return err
		}
		for i, assumption := range document.Assumptions {
			for j, id := range assumption.EvidenceIDs {
				if _, ok := evidenceIDs[id]; !ok {
					return fmt.Errorf("assumptions[%d].evidence_ids[%d] %q does not reference evidence", i, j, id)
				}
			}
		}
	}
	return nil
}

// ValidatePreviewLineage validates the semantic identity fields for one
// ui_preview_lineage.v1 record.
func ValidatePreviewLineage(documentData []byte) error {
	var document previewLineageDocument
	if err := decodeJSON(documentData, &document); err != nil {
		return err
	}
	if document.LayoutRevision < 1 {
		return fmt.Errorf("layout_revision must be at least 1")
	}
	if len(document.TargetMatrix.Targets) == 0 {
		return fmt.Errorf("target_matrix.targets must not be empty")
	}
	if document.Approval.State == "user_confirmed" && document.Approval.EvidenceRef == "" {
		return fmt.Errorf("approval.evidence_ref is required for user_confirmed lineage")
	}
	return nil
}

// ValidateViewImageResult validates the source-primary and crop-child contract
// for view_image_result.v1.
func ValidateViewImageResult(documentData []byte) error {
	var document viewImageResultDocument
	if err := decodeJSON(documentData, &document); err != nil {
		return err
	}
	if len(document.Assets) == 0 {
		return fmt.Errorf("assets must contain a source image")
	}
	if document.Assets[0].Role != "source" {
		return fmt.Errorf("assets[0] must be the source image")
	}
	if document.Assets[0].AssetID != document.SourceAssetID {
		return fmt.Errorf("source_asset_id does not match assets[0].asset_id")
	}

	assetIDs := make(map[string]struct{}, len(document.Assets))
	sourceCount := 0
	cropCount := 0
	for i, asset := range document.Assets {
		if err := addUniqueID(assetIDs, asset.AssetID, fmt.Sprintf("assets[%d].asset_id", i)); err != nil {
			return err
		}
		switch asset.Role {
		case "source":
			sourceCount++
		case "crop":
			cropCount++
		default:
			return fmt.Errorf("assets[%d].role %q is invalid", i, asset.Role)
		}
	}
	if sourceCount != 1 {
		return fmt.Errorf("assets must contain exactly one source image")
	}
	if cropCount > 1 {
		return fmt.Errorf("assets must contain at most one crop image")
	}

	if document.DetailEffective == "original" && document.DetailRequested != "original" {
		return fmt.Errorf("detail_effective cannot be original unless original detail was requested")
	}
	if document.DetailRequested == "original" && document.DetailEffective != "original" {
		return fmt.Errorf("original detail request must fail or preserve original detail")
	}

	if document.CropAssetID == nil {
		if len(document.Assets) != 1 || cropCount != 0 {
			return fmt.Errorf("crop_asset_id is null but crop assets are present")
		}
		if document.SourceRegion != nil {
			return fmt.Errorf("source_region requires a crop")
		}
		return nil
	}
	if len(document.Assets) != 2 || cropCount != 1 {
		return fmt.Errorf("crop_asset_id requires exactly one crop asset")
	}
	crop := document.Assets[1]
	if crop.AssetID != *document.CropAssetID {
		return fmt.Errorf("crop_asset_id does not match assets[1].asset_id")
	}
	if crop.SourceAssetID != document.SourceAssetID {
		return fmt.Errorf("crop source_asset_id does not match the primary source asset")
	}
	if crop.SourceBBox == nil {
		return fmt.Errorf("crop source_bbox is required")
	}
	if err := validateBBox("crop source_bbox", *crop.SourceBBox); err != nil {
		return err
	}
	if document.SourceRegion == nil {
		return fmt.Errorf("source_region is required when crop_asset_id is present")
	}
	if err := validateBBox("source_region", *document.SourceRegion); err != nil {
		return err
	}
	if *crop.SourceBBox != *document.SourceRegion {
		return fmt.Errorf("crop source_bbox must match source_region")
	}
	return nil
}

type bbox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type evidenceDocument struct {
	Source         evidenceSource    `json:"source"`
	Elements       []evidenceElement `json:"elements"`
	RepeatedGroups []repeatedGroup   `json:"repeated_groups"`
	SourceTree     sourceTree        `json:"source_tree"`
	Crops          []crop            `json:"crops"`
	Uncertainties  []uncertainty     `json:"uncertainties"`
	Limits         evidenceLimits    `json:"limits"`
}

type evidenceSource struct {
	AssetID string `json:"asset_id"`
	SHA256  string `json:"sha256"`
}

type evidenceElement struct {
	ID       string  `json:"id"`
	BBox     bbox    `json:"bbox"`
	ParentID *string `json:"parent_id"`
}

type repeatedGroup struct {
	ID        string   `json:"id"`
	MemberIDs []string `json:"member_ids"`
	BBox      bbox     `json:"bbox"`
}

type sourceTree struct {
	Kind  string       `json:"kind"`
	Nodes []sourceNode `json:"nodes"`
}

type sourceNode struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parent_id"`
	BBox     *bbox   `json:"bbox"`
}

type crop struct {
	AssetID    string `json:"asset_id"`
	SourceBBox bbox   `json:"source_bbox"`
}

type uncertainty struct {
	RelatedElementIDs []string `json:"related_element_ids"`
}

type evidenceLimits struct {
	ElementCount int `json:"element_count"`
}

type layoutDocument struct {
	Source      layoutSource       `json:"source"`
	Root        layoutNode         `json:"root"`
	Responsive  layoutResponsive   `json:"responsive"`
	Assumptions []layoutAssumption `json:"assumptions"`
}

type layoutSource struct {
	AssetID           string `json:"asset_id"`
	SourceAssetSHA256 string `json:"source_asset_sha256"`
}

type layoutNode struct {
	ID          string       `json:"id"`
	BBox        *bbox        `json:"bbox"`
	Children    []layoutNode `json:"children"`
	EvidenceIDs []string     `json:"evidence_ids"`
}

type layoutResponsive struct {
	Invariants   []layoutInvariant `json:"invariants"`
	NodePolicies []nodePolicy      `json:"node_policies"`
}

type layoutInvariant struct {
	ID string `json:"id"`
}

type nodePolicy struct {
	NodeID string `json:"node_id"`
}

type layoutAssumption struct {
	EvidenceIDs []string `json:"evidence_ids"`
}

type previewLineageDocument struct {
	LayoutRevision int64 `json:"layout_revision"`
	Approval       struct {
		State       string `json:"state"`
		EvidenceRef string `json:"evidence_ref"`
	} `json:"approval"`
	TargetMatrix struct {
		Targets []string `json:"targets"`
	} `json:"target_matrix"`
}

type viewImageResultDocument struct {
	SourceAssetID   string           `json:"source_asset_id"`
	CropAssetID     *string          `json:"crop_asset_id"`
	Assets          []viewImageAsset `json:"assets"`
	DetailRequested string           `json:"detail_requested"`
	DetailEffective string           `json:"detail_effective"`
	SourceRegion    *bbox            `json:"source_region"`
}

type viewImageAsset struct {
	AssetID       string `json:"asset_id"`
	Role          string `json:"role"`
	SourceAssetID string `json:"source_asset_id"`
	SourceBBox    *bbox  `json:"source_bbox"`
}

func walkLayoutNode(node layoutNode, ids map[string]struct{}, count *int) error {
	(*count)++
	if err := addUniqueID(ids, node.ID, "layout node id"); err != nil {
		return err
	}
	if node.BBox != nil {
		if err := validateBBox("layout node "+node.ID+" bbox", *node.BBox); err != nil {
			return err
		}
	}
	for _, child := range node.Children {
		if err := walkLayoutNode(child, ids, count); err != nil {
			return err
		}
	}
	return nil
}

func checkLayoutEvidenceReferences(node layoutNode, evidenceIDs map[string]struct{}) error {
	for _, id := range node.EvidenceIDs {
		if _, ok := evidenceIDs[id]; !ok {
			return fmt.Errorf("layout node %q evidence id %q does not reference evidence", node.ID, id)
		}
	}
	for _, child := range node.Children {
		if err := checkLayoutEvidenceReferences(child, evidenceIDs); err != nil {
			return err
		}
	}
	return nil
}

func validateBBox(path string, box bbox) error {
	values := []float64{box.X, box.Y, box.Width, box.Height}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s contains a non-finite coordinate", path)
		}
		if value < 0 || value > 1 {
			return fmt.Errorf("%s coordinate %v is outside [0,1]", path, value)
		}
	}
	const epsilon = 1e-9
	if box.X+box.Width > 1+epsilon || box.Y+box.Height > 1+epsilon {
		return fmt.Errorf("%s exceeds normalized canvas bounds", path)
	}
	if box.Width == 0 && box.Height == 0 {
		return fmt.Errorf("%s has zero width and height", path)
	}
	return nil
}

func addUniqueID(ids map[string]struct{}, id, path string) error {
	if _, exists := ids[id]; exists {
		return fmt.Errorf("duplicate id %q at %s", id, path)
	}
	ids[id] = struct{}{}
	return nil
}

func decodeJSON(data []byte, target any) error {
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
