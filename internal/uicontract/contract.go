// Package uicontract validates the executable JSON contracts used by visual
// layout understanding. It checks JSON Schema shape plus cross-reference rules
// that JSON Schema cannot express directly.
package uicontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	pathpkg "path"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

var previewArtifactKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,191}$`)

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
	if len(evidenceData) == 0 {
		if document.Source.EvidenceSHA256 != "" {
			return fmt.Errorf("source.evidence_sha256 requires matching evidence")
		}
		if err := requireNoLayoutEvidenceReferences(document); err != nil {
			return err
		}
		return nil
	}

	var evidence evidenceDocument
	if err := decodeJSON(evidenceData, &evidence); err != nil {
		return fmt.Errorf("decode evidence: %w", err)
	}
	if err := ValidateUIEvidence(evidenceData); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	if document.Source.EvidenceSHA256 != "" {
		sum := sha256.Sum256(evidenceData)
		if hex.EncodeToString(sum[:]) != document.Source.EvidenceSHA256 {
			return fmt.Errorf("source.evidence_sha256 does not match the evidence bytes")
		}
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
	return nil
}

func requireNoLayoutEvidenceReferences(document layoutDocument) error {
	nodes := []layoutNode{document.Root}
	for len(nodes) > 0 {
		node := nodes[len(nodes)-1]
		nodes = nodes[:len(nodes)-1]
		if len(node.EvidenceIDs) > 0 {
			return fmt.Errorf("layout node %q references evidence but no ui_evidence.v1 document was supplied", node.ID)
		}
		nodes = append(nodes, node.Children...)
	}
	for i, assumption := range document.Assumptions {
		if len(assumption.EvidenceIDs) > 0 {
			return fmt.Errorf("assumptions[%d] references evidence but no ui_evidence.v1 document was supplied", i)
		}
	}
	return nil
}

// ValidatePreviewLineage validates the semantic identity fields for one
// ui_preview_lineage.v1 record. It assumes the document has already passed the
// frozen schema; the cross-field rules below cannot be expressed in JSON Schema.
func ValidatePreviewLineage(documentData []byte) error {
	var document previewLineageDocument
	if err := decodeJSON(documentData, &document); err != nil {
		return err
	}
	if document.Schema != "ui_preview_lineage.v1" {
		return fmt.Errorf("schema must be ui_preview_lineage.v1")
	}
	if !strings.HasPrefix(document.LayoutArtifactID, "layout_") {
		return fmt.Errorf("layout_artifact_id must use the layout_ prefix")
	}
	if document.LayoutRevision < 1 {
		return fmt.Errorf("layout_revision must be at least 1")
	}
	if _, ok := revisionDigest(document.LayoutRevisionSHA256); !ok {
		return fmt.Errorf("layout_revision_sha256 must be sha256:<64 lowercase hex>")
	}
	if !strings.HasPrefix(document.SourceAssetID, "asset_") {
		return fmt.Errorf("source_asset_id must use the asset_ prefix")
	}
	if !isLowerHexSHA256(document.SourceAssetSHA256) {
		return fmt.Errorf("source_asset_sha256 must be 64 lowercase hex characters")
	}
	if document.EvidenceSHA256 != nil && !isLowerHexSHA256(*document.EvidenceSHA256) {
		return fmt.Errorf("evidence_sha256 must be null or 64 lowercase hex characters")
	}
	if strings.TrimSpace(document.Target) == "" {
		return fmt.Errorf("target is required")
	}
	state := strings.TrimSpace(document.State)
	if state == "" || len([]rune(state)) > 64 {
		return fmt.Errorf("state must contain 1 to 64 characters")
	}
	if !strings.HasPrefix(document.TargetMatrix.ID, "matrix_") {
		return fmt.Errorf("target_matrix.id must use the matrix_ prefix")
	}
	if len(document.TargetMatrix.Targets) == 0 {
		return fmt.Errorf("target_matrix.targets must not be empty")
	}
	seen := make(map[string]struct{}, len(document.TargetMatrix.Targets))
	targetFound := false
	for _, target := range document.TargetMatrix.Targets {
		if _, exists := seen[target]; exists {
			return fmt.Errorf("target_matrix.targets contains duplicate %q", target)
		}
		seen[target] = struct{}{}
		if target == document.Target {
			targetFound = true
		}
	}
	if !targetFound {
		return fmt.Errorf("target %q is not included in target_matrix.targets", document.Target)
	}
	if document.Approval.State == "user_confirmed" && document.Approval.EvidenceRef == "" {
		return fmt.Errorf("approval.evidence_ref is required for user_confirmed lineage")
	}
	return nil
}

// ValidateUIPreviewResult validates the stable ui_preview_result.v1 side-channel.
// Lineage presence is bound to the preview mode, and a non-null lineage must be
// a valid ui_preview_lineage.v1 record.
func ValidateUIPreviewResult(documentData []byte) error {
	var document previewResultDocument
	if err := decodeJSON(documentData, &document); err != nil {
		return err
	}
	if document.Schema != "ui_preview_result.v1" {
		return fmt.Errorf("schema must be ui_preview_result.v1")
	}
	if !previewArtifactKeyPattern.MatchString(document.ArtifactKey) {
		return fmt.Errorf("artifact_key is invalid")
	}
	if document.Kind != "interactive_html" && document.Kind != "static_image" {
		return fmt.Errorf("kind %q is unsupported", document.Kind)
	}
	title := strings.TrimSpace(document.Title)
	if title == "" || len([]rune(title)) > 256 {
		return fmt.Errorf("title must contain 1 to 256 characters")
	}
	hasLineage := len(document.Lineage) > 0 && string(document.Lineage) != "null"
	switch document.Mode {
	case "adaptation":
		if !hasLineage {
			return fmt.Errorf("adaptation preview requires ui_preview_lineage.v1")
		}
		if err := ValidatePreviewLineage(document.Lineage); err != nil {
			return fmt.Errorf("lineage: %w", err)
		}
	case "explore":
		if hasLineage {
			return fmt.Errorf("explore preview must not carry lineage")
		}
	default:
		return fmt.Errorf("preview mode %q is unsupported", document.Mode)
	}
	if !strings.HasPrefix(document.FallbackAsset.AssetID, "asset_") {
		return fmt.Errorf("fallback_asset.asset_id must use the asset_ prefix")
	}
	if err := validateWorkspaceRelativePath("fallback_asset.relative_path", document.FallbackAsset.RelativePath); err != nil {
		return err
	}
	switch document.FallbackAsset.MIMEType {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return fmt.Errorf("fallback_asset.mime_type %q is unsupported", document.FallbackAsset.MIMEType)
	}
	if document.FallbackAsset.Width < 0 || document.FallbackAsset.Height < 0 {
		return fmt.Errorf("fallback_asset dimensions must not be negative")
	}
	if !isLowerHexSHA256(document.FallbackAsset.SHA256) {
		return fmt.Errorf("fallback_asset.sha256 must be 64 lowercase hex characters")
	}
	return nil
}

func validateWorkspaceRelativePath(field, value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, "\\") || pathpkg.IsAbs(value) {
		return fmt.Errorf("%s must be a workspace-relative slash path", field)
	}
	cleaned := pathpkg.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("%s must not escape the workspace", field)
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
	EvidenceSchema    string `json:"evidence_schema"`
	EvidenceSHA256    string `json:"evidence_sha256"`
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
	Schema               string  `json:"schema"`
	LayoutArtifactID     string  `json:"layout_artifact_id"`
	LayoutRevision       int64   `json:"layout_revision"`
	LayoutRevisionSHA256 string  `json:"layout_revision_sha256"`
	SourceAssetID        string  `json:"source_asset_id"`
	SourceAssetSHA256    string  `json:"source_asset_sha256"`
	EvidenceSHA256       *string `json:"evidence_sha256"`
	Approval             struct {
		State       string `json:"state"`
		EvidenceRef string `json:"evidence_ref"`
	} `json:"approval"`
	Target       string `json:"target"`
	State        string `json:"state"`
	TargetMatrix struct {
		ID      string   `json:"id"`
		Targets []string `json:"targets"`
	} `json:"target_matrix"`
}

type previewResultDocument struct {
	Schema        string          `json:"schema"`
	ArtifactKey   string          `json:"artifact_key"`
	Kind          string          `json:"kind"`
	Title         string          `json:"title"`
	Mode          string          `json:"mode"`
	Lineage       json.RawMessage `json:"lineage"`
	FallbackAsset struct {
		AssetID      string `json:"asset_id"`
		RelativePath string `json:"relative_path"`
		MIMEType     string `json:"mime_type"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		SHA256       string `json:"sha256"`
	} `json:"fallback_asset"`
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

// ValidateUILayoutResult validates identity, asset, and digest consistency for
// ui_layout_result.v1.
func ValidateUILayoutResult(documentData []byte) error {
	var document layoutResultDocument
	if err := decodeJSON(documentData, &document); err != nil {
		return err
	}
	if document.Schema != "ui_layout_result.v1" {
		return fmt.Errorf("schema must be ui_layout_result.v1")
	}
	if !strings.HasPrefix(document.LayoutArtifactID, "layout_") {
		return fmt.Errorf("layout_artifact_id must use the layout_ prefix")
	}
	if document.LayoutRevision < 1 {
		return fmt.Errorf("layout_revision must be at least 1")
	}
	layoutDigest, ok := revisionDigest(document.LayoutRevisionSHA256)
	if !ok {
		return fmt.Errorf("layout_revision_sha256 must be sha256:<64 lowercase hex>")
	}
	if !isLowerHexSHA256(document.SourceAssetSHA256) {
		return fmt.Errorf("source_asset_sha256 must be 64 lowercase hex characters")
	}
	if !strings.HasPrefix(document.SourceAssetID, "asset_") {
		return fmt.Errorf("source_asset_id must use the asset_ prefix")
	}
	if document.EvidenceSHA256 != nil && !isLowerHexSHA256(*document.EvidenceSHA256) {
		return fmt.Errorf("evidence_sha256 must be null or 64 lowercase hex characters")
	}
	if document.Canvas.Width < 1 || document.Canvas.Height < 1 {
		return fmt.Errorf("canvas dimensions must be positive")
	}
	if document.NodeCount < 1 {
		return fmt.Errorf("node_count must be at least 1")
	}
	if document.AssumptionCount < 0 {
		return fmt.Errorf("assumption_count must not be negative")
	}
	if len(document.Targets) == 0 {
		return fmt.Errorf("targets must not be empty")
	}
	if !sort.StringsAreSorted(document.Targets) {
		return fmt.Errorf("targets must be sorted for deterministic output")
	}
	for i := 1; i < len(document.Targets); i++ {
		if document.Targets[i] == document.Targets[i-1] {
			return fmt.Errorf("targets contains duplicate %q", document.Targets[i])
		}
	}

	if document.OverlayAsset.AssetID == document.LayoutJSONAsset.AssetID {
		return fmt.Errorf("overlay_asset.asset_id must differ from layout_json_asset.asset_id")
	}
	if err := validateLayoutResultAsset("overlay_asset", document.OverlayAsset, "layout_overlay"); err != nil {
		return err
	}
	if err := validateLayoutResultAsset("layout_json_asset", document.LayoutJSONAsset, "layout_json"); err != nil {
		return err
	}
	if document.OverlayAsset.Kind != "image" || document.OverlayAsset.MIMEType != "image/png" {
		return fmt.Errorf("overlay_asset must be an image/png asset")
	}
	if document.LayoutJSONAsset.Kind != "file" || document.LayoutJSONAsset.MIMEType != "application/json" {
		return fmt.Errorf("layout_json_asset must be an application/json file asset")
	}
	if document.OverlayAsset.Width < 1 || document.OverlayAsset.Height < 1 {
		return fmt.Errorf("overlay_asset dimensions must be positive")
	}
	if document.OverlayAsset.Width > document.Canvas.Width || document.OverlayAsset.Height > document.Canvas.Height {
		return fmt.Errorf("overlay_asset must not exceed the source canvas")
	}
	if document.LayoutJSONAsset.Width != 0 || document.LayoutJSONAsset.Height != 0 {
		return fmt.Errorf("layout_json_asset must not declare image dimensions")
	}
	if document.OverlayAsset.RelativePath == document.LayoutJSONAsset.RelativePath {
		return fmt.Errorf("overlay_asset and layout_json_asset must use distinct paths")
	}
	if document.LayoutJSONAsset.SHA256 != layoutDigest {
		return fmt.Errorf("layout_json_asset.sha256 must match layout_revision_sha256")
	}
	return nil
}

func validateLayoutResultAsset(path string, asset layoutResultAsset, role string) error {
	if asset.AssetRole != role {
		return fmt.Errorf("%s.asset_role must be %q", path, role)
	}
	if !strings.HasPrefix(asset.AssetID, "asset_") {
		return fmt.Errorf("%s.asset_id must use the asset_ prefix", path)
	}
	if !isLowerHexSHA256(asset.SHA256) {
		return fmt.Errorf("%s.sha256 must be 64 lowercase hex characters", path)
	}
	if asset.RelativePath == "" || strings.TrimSpace(asset.RelativePath) != asset.RelativePath ||
		strings.Contains(asset.RelativePath, "\\") || pathpkg.IsAbs(asset.RelativePath) {
		return fmt.Errorf("%s.relative_path must be a workspace-relative slash path", path)
	}
	cleaned := pathpkg.Clean(asset.RelativePath)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("%s.relative_path must not escape the workspace", path)
	}
	if pathpkg.Base(asset.RelativePath) != asset.Filename {
		return fmt.Errorf("%s.filename must match the relative_path basename", path)
	}
	return nil
}

func revisionDigest(value string) (string, bool) {
	if !strings.HasPrefix(value, "sha256:") {
		return "", false
	}
	digest := strings.TrimPrefix(value, "sha256:")
	return digest, isLowerHexSHA256(digest)
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return false
	}
	return true
}

type layoutResultDocument struct {
	Schema               string             `json:"schema"`
	LayoutArtifactID     string             `json:"layout_artifact_id"`
	LayoutRevision       int                `json:"layout_revision"`
	LayoutRevisionSHA256 string             `json:"layout_revision_sha256"`
	SourceAssetID        string             `json:"source_asset_id"`
	SourceAssetSHA256    string             `json:"source_asset_sha256"`
	EvidenceSHA256       *string            `json:"evidence_sha256"`
	Canvas               layoutResultCanvas `json:"canvas"`
	NodeCount            int                `json:"node_count"`
	AssumptionCount      int                `json:"assumption_count"`
	Targets              []string           `json:"targets"`
	Created              bool               `json:"created"`
	OverlayAsset         layoutResultAsset  `json:"overlay_asset"`
	LayoutJSONAsset      layoutResultAsset  `json:"layout_json_asset"`
}

type layoutResultCanvas struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type layoutResultAsset struct {
	AssetID      string `json:"asset_id"`
	AssetRole    string `json:"asset_role"`
	Kind         string `json:"kind"`
	RelativePath string `json:"relative_path"`
	Filename     string `json:"filename"`
	MIMEType     string `json:"mime_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	SHA256       string `json:"sha256"`
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
