// Package imageview implements the built-in view_image tool. It loads a
// workspace image, validates it, and returns image assets for model input. It
// deliberately performs no OCR or visual interpretation.
package imageview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	assets "code-agent/internal/assetref"
	"code-agent/internal/tools"
	"code-agent/internal/workspace"
)

const (
	DefaultMaxImageBytes  int64 = 32 << 20
	DefaultMaxTotalBytes  int64 = 32 << 20
	DefaultMaxImagePixels int64 = 40_000_000
	DefaultMaxTotalPixels int64 = 40_000_000
)

// Tool loads a workspace image and exposes it as image assets. Its output is
// view_image_result.v1; it never recommends components or layout decisions.
type Tool struct {
	MaxImageBytes  int64
	MaxTotalBytes  int64
	MaxImagePixels int64
	MaxTotalPixels int64
}

// NewTool returns a view_image tool with the P0 resource budgets.
func NewTool() *Tool {
	return &Tool{
		MaxImageBytes:  DefaultMaxImageBytes,
		MaxTotalBytes:  DefaultMaxTotalBytes,
		MaxImagePixels: DefaultMaxImagePixels,
		MaxTotalPixels: DefaultMaxTotalPixels,
	}
}

func (*Tool) Name() string { return "view_image" }

func (*Tool) Description() string {
	return "Load one workspace image and expose its pixels as image content for the current model. " +
		"Supports PNG, JPEG, and WebP input, optional normalized region crops for PNG/JPEG, and high or original detail. " +
		"Requires image input and tool-result image input. This tool does not OCR, infer layout, or choose components."
}

func (*Tool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"path": {
			Type:        "string",
			Description: "Workspace-relative image path. Absolute paths and traversal are rejected.",
		},
		"detail": {
			Type:        "string",
			Description: "Image detail: high (default) or original. Original requires image_detail_original support.",
			Enum:        []string{"high", "original"},
		},
		"region": {
			Type:        "object",
			Description: "Optional normalized top-left crop region in the original image. Requires x, y, width, and height in [0,1].",
			Properties: map[string]tools.Property{
				"x":      {Type: "number", Description: "Normalized left edge."},
				"y":      {Type: "number", Description: "Normalized top edge."},
				"width":  {Type: "number", Description: "Normalized crop width."},
				"height": {Type: "number", Description: "Normalized crop height."},
			},
			Required: []string{"x", "y", "width", "height"},
		},
	}, "path").Closed().JSON()
}

type viewInput struct {
	Path   string         `json:"path"`
	Detail string         `json:"detail"`
	Region *normalizedBox `json:"region,omitempty"`
}

type normalizedBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type resultAsset struct {
	AssetID       string         `json:"asset_id"`
	AssetRole     string         `json:"asset_role"`
	Role          string         `json:"role"`
	RelativePath  string         `json:"relative_path"`
	Filename      string         `json:"filename"`
	MIMEType      string         `json:"mime_type"`
	Width         int            `json:"width"`
	Height        int            `json:"height"`
	SHA256        string         `json:"sha256"`
	SourceAssetID string         `json:"source_asset_id,omitempty"`
	SourceBBox    *normalizedBox `json:"source_bbox,omitempty"`
	Purpose       string         `json:"purpose,omitempty"`
}

type viewResult struct {
	Schema          string         `json:"schema"`
	Kind            string         `json:"kind"`
	SourceAssetID   string         `json:"source_asset_id"`
	CropAssetID     *string        `json:"crop_asset_id"`
	Assets          []resultAsset  `json:"assets"`
	DetailRequested string         `json:"detail_requested"`
	DetailEffective string         `json:"detail_effective"`
	SourceRegion    *normalizedBox `json:"source_region"`
}

type codedError struct {
	kind    string
	message string
}

func (e *codedError) Error() string { return e.message }

func coded(kind, message string) error {
	return &codedError{kind: kind, message: message}
}

func (t *Tool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return tools.ToolResult{}, ctx.Err()
		default:
		}
	}
	maxImageBytes := t.MaxImageBytes
	if maxImageBytes <= 0 {
		maxImageBytes = DefaultMaxImageBytes
	}
	maxTotalBytes := t.MaxTotalBytes
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultMaxTotalBytes
	}
	maxImagePixels := t.MaxImagePixels
	if maxImagePixels <= 0 {
		maxImagePixels = DefaultMaxImagePixels
	}
	maxTotalPixels := t.MaxTotalPixels
	if maxTotalPixels <= 0 {
		maxTotalPixels = DefaultMaxTotalPixels
	}

	var in viewInput
	if err := decodeStrict(raw, &in); err != nil {
		return tools.FailureResult("invalid_arguments", "view_image input is invalid: "+err.Error()), nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return tools.FailureResult("invalid_arguments", "view_image requires path"), nil
	}
	if in.Detail == "" {
		in.Detail = "high"
	}
	if in.Detail != "high" && in.Detail != "original" {
		return tools.FailureResult("invalid_arguments", "view_image detail must be high or original"), nil
	}
	if in.Region != nil {
		if err := validateNormalizedBox(*in.Region); err != nil {
			return tools.FailureResult("invalid_arguments", "view_image region is invalid: "+err.Error()), nil
		}
	}

	if !ec.ImageCapabilities.Input {
		message := "The selected model cannot process image input."
		if ec.ImageCapabilities.VisionBridge {
			message += " An explicit vision bridge is available but view_image does not use it automatically."
		}
		return tools.FailureResult("image_input_unsupported", message), nil
	}
	if !ec.ImageCapabilities.ToolInput {
		message := "The selected provider cannot process images returned inside tool results."
		if ec.ImageCapabilities.VisionBridge {
			message += " An explicit vision bridge is available but view_image does not use it automatically."
		}
		return tools.FailureResult("tool_image_input_unsupported", message), nil
	}
	if in.Detail == "original" && !ec.ImageCapabilities.OriginalDetail {
		return tools.FailureResult("image_detail_original_unsupported", "The selected model/provider cannot preserve original image detail; retry with detail=high."), nil
	}

	sourceAbs, sourceRel, err := resolveWorkspaceFile(ec.WorkspaceRoot, in.Path)
	if err != nil {
		return failureFromError(err), nil
	}
	data, err := readBoundedFile(sourceAbs, maxImageBytes)
	if err != nil {
		return failureFromError(err), nil
	}
	mimeType := sniffMIMEType(data)
	if mimeType == "" {
		return tools.FailureResult("unsupported_format", "view_image supports PNG, JPEG, and WebP images only."), nil
	}
	width, height, err := inspectImage(data, mimeType)
	if err != nil {
		return tools.FailureResult("invalid_image", "view_image could not validate the image: "+err.Error()), nil
	}
	if int64(width)*int64(height) > maxImagePixels {
		return tools.FailureResult("image_too_large", fmt.Sprintf("view_image image has %d pixels; maximum is %d.", int64(width)*int64(height), maxImagePixels)), nil
	}

	sourceSHA := digestBytes(data)
	sourceID := assets.StableID(ec.TurnID, ec.CallID, 0, sourceSHA, "view_image_source")
	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}
	workspaceID := assets.WorkspaceID(rootAbs)
	sourceRef := assets.Ref{
		ID:                    sourceID,
		Kind:                  "image",
		URI:                   assets.WorkspaceURI(workspaceID, sourceRel, nil),
		DisplayName:           filepath.Base(sourceRel),
		WorkspaceID:           workspaceID,
		WorkspaceRelativePath: sourceRel,
		AbsolutePath:          sourceAbs,
		MIMEType:              mimeType,
		Metadata:              map[string]any{"asset_role": "source"},
		SourceTurnID:          ec.TurnID,
		SourceCallID:          ec.CallID,
	}
	output := viewResult{
		Schema:          "view_image_result.v1",
		Kind:            "image_view",
		SourceAssetID:   sourceID,
		DetailRequested: in.Detail,
		DetailEffective: in.Detail,
		Assets: []resultAsset{{
			AssetID:      sourceID,
			AssetRole:    "source",
			Role:         "source",
			RelativePath: sourceRel,
			Filename:     filepath.Base(sourceRel),
			MIMEType:     mimeType,
			Width:        width,
			Height:       height,
			SHA256:       sourceSHA,
		}},
	}
	refs := []assets.Ref{sourceRef}
	totalBytes := int64(len(data))
	totalPixels := int64(width) * int64(height)

	if in.Region != nil {
		if mimeType == "image/webp" {
			return tools.FailureResult("crop_unsupported", "Region crops are not supported for WebP in P0; view the full WebP source without a region."), nil
		}
		cropData, cropWidth, cropHeight, err := encodeCrop(data, mimeType, *in.Region)
		if err != nil {
			return tools.FailureResult("invalid_image", "view_image could not crop the image: "+err.Error()), nil
		}
		totalBytes += int64(len(cropData))
		totalPixels += int64(cropWidth) * int64(cropHeight)
		if totalBytes > maxTotalBytes {
			return tools.FailureResult("image_too_large", fmt.Sprintf("view_image source plus crop exceeds the %d MiB request budget.", maxTotalBytes>>20)), nil
		}
		if totalPixels > maxTotalPixels {
			return tools.FailureResult("image_too_large", fmt.Sprintf("view_image source plus crop has %d pixels; maximum is %d.", totalPixels, maxTotalPixels)), nil
		}
		cropSHA := digestBytes(cropData)
		cropID := assets.StableID(ec.TurnID, ec.CallID, 1, cropSHA, "view_image_crop")
		cropAbs, cropRel, err := writeControlledImage(rootAbs, cropID, cropData)
		if err != nil {
			return tools.ToolResult{}, err
		}
		cropRef := assets.Ref{
			ID:                    cropID,
			Kind:                  "image",
			URI:                   assets.WorkspaceURI(workspaceID, cropRel, nil),
			DisplayName:           filepath.Base(cropRel),
			WorkspaceID:           workspaceID,
			WorkspaceRelativePath: cropRel,
			AbsolutePath:          cropAbs,
			MIMEType:              "image/png",
			Metadata: map[string]any{
				"asset_role":      "crop",
				"source_asset_id": sourceID,
				"source_bbox":     *in.Region,
				"purpose":         "unknown",
			},
			SourceTurnID: ec.TurnID,
			SourceCallID: ec.CallID,
		}
		refs = append(refs, cropRef)
		output.CropAssetID = &cropID
		output.SourceRegion = in.Region
		output.Assets = append(output.Assets, resultAsset{
			AssetID:       cropID,
			AssetRole:     "crop",
			Role:          "crop",
			RelativePath:  cropRel,
			Filename:      filepath.Base(cropRel),
			MIMEType:      "image/png",
			Width:         cropWidth,
			Height:        cropHeight,
			SHA256:        cropSHA,
			SourceAssetID: sourceID,
			SourceBBox:    in.Region,
			Purpose:       "unknown",
		})
	}

	outputJSON, err := json.Marshal(output)
	if err != nil {
		return tools.ToolResult{}, err
	}
	content := fmt.Sprintf("Viewed image %s (%dx%d, %s detail).", filepath.Base(sourceRel), width, height, in.Detail)
	if output.CropAssetID != nil {
		content = fmt.Sprintf("Viewed image %s (%dx%d, %s detail) and cropped region.", filepath.Base(sourceRel), width, height, in.Detail)
	}
	return tools.ToolResult{Content: content, Output: outputJSON, Assets: refs}, nil
}

func decodeStrict(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return fmt.Errorf("input is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func resolveWorkspaceFile(root, inputPath string) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", coded("unavailable", "view_image requires a workspace root")
	}
	pathValue := strings.TrimSpace(inputPath)
	if filepath.IsAbs(pathValue) || strings.HasPrefix(pathValue, "/") || strings.Contains(pathValue, "\\") {
		return "", "", coded("blocked", "view_image path must be workspace-relative and use forward slashes")
	}
	if strings.IndexFunc(pathValue, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", "", coded("blocked", "view_image path contains control characters")
	}
	clean := filepath.Clean(filepath.FromSlash(pathValue))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", coded("blocked", "view_image path escapes the workspace")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	target := workspace.ResolveToolPath(rootAbs, clean)
	switch workspace.ClassifyPath(rootAbs, target) {
	case workspace.PathInsideWorkspace:
	case workspace.PathManagedWorktree:
		return "", "", coded("blocked", "view_image cannot read the managed worktree tree")
	default:
		return "", "", coded("blocked", "view_image path escapes the workspace")
	}
	target, err = workspace.CanonicalPath(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", coded("not_found", "view_image image was not found: "+inputPath)
		}
		return "", "", coded("blocked", "view_image path could not be resolved: "+err.Error())
	}
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", coded("not_found", "view_image image was not found: "+inputPath)
		}
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", coded("invalid_arguments", "view_image requires a regular image file")
	}
	rootCanonical, err := workspace.CanonicalPath(rootAbs)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(rootCanonical, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", coded("blocked", "view_image path escapes the workspace")
	}
	return target, filepath.ToSlash(rel), nil
}

func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, coded("not_found", "view_image image was not found")
		}
		return nil, err
	}
	defer file.Close()
	if maxBytes > 0 {
		data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxBytes {
			return nil, coded("image_too_large", fmt.Sprintf("view_image image exceeds the %d MiB file budget.", maxBytes>>20))
		}
		return data, nil
	}
	return io.ReadAll(file)
}

func sniffMIMEType(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

func inspectImage(data []byte, mimeType string) (int, int, error) {
	switch mimeType {
	case "image/png":
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, err
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			return 0, 0, fmt.Errorf("PNG has invalid dimensions")
		}
		return cfg.Width, cfg.Height, nil
	case "image/jpeg":
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, err
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			return 0, 0, fmt.Errorf("JPEG has invalid dimensions")
		}
		return cfg.Width, cfg.Height, nil
	case "image/webp":
		return inspectWebP(data)
	default:
		return 0, 0, fmt.Errorf("unsupported image MIME type %q", mimeType)
	}
}

func inspectWebP(data []byte) (int, int, error) {
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, fmt.Errorf("WebP container is invalid")
	}
	riffSize := int(binary.LittleEndian.Uint32(data[4:8])) + 8
	if riffSize < 20 || riffSize > len(data) {
		return 0, 0, fmt.Errorf("WebP container is truncated")
	}
	for offset := 12; offset+8 <= riffSize; {
		chunk := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + size
		if end > riffSize {
			return 0, 0, fmt.Errorf("WebP chunk is truncated")
		}
		payload := data[start:end]
		switch chunk {
		case "VP8X":
			if len(payload) < 10 {
				return 0, 0, fmt.Errorf("WebP extended header is truncated")
			}
			width := 1 + int(uint32(payload[4])|uint32(payload[5])<<8|uint32(payload[6])<<16)
			height := 1 + int(uint32(payload[7])|uint32(payload[8])<<8|uint32(payload[9])<<16)
			return width, height, nil
		case "VP8 ":
			if len(payload) < 10 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
				return 0, 0, fmt.Errorf("WebP lossy frame is invalid")
			}
			width := int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
			height := int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)
			if width == 0 || height == 0 {
				return 0, 0, fmt.Errorf("WebP lossy frame has invalid dimensions")
			}
			return width, height, nil
		case "VP8L":
			if len(payload) < 5 || payload[0] != 0x2f {
				return 0, 0, fmt.Errorf("WebP lossless header is invalid")
			}
			bits := binary.LittleEndian.Uint32(payload[1:5])
			width := 1 + int(bits&0x3fff)
			height := 1 + int((bits>>14)&0x3fff)
			return width, height, nil
		}
		offset = end
		if size%2 == 1 {
			offset++
		}
	}
	return 0, 0, fmt.Errorf("WebP has no supported image frame")
}

func validateNormalizedBox(box normalizedBox) error {
	values := []float64{box.X, box.Y, box.Width, box.Height}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("coordinates must be finite")
		}
		if value < 0 || value > 1 {
			return fmt.Errorf("coordinates must be within [0,1]")
		}
	}
	if box.Width <= 0 || box.Height <= 0 {
		return fmt.Errorf("width and height must be positive")
	}
	const epsilon = 1e-9
	if box.X+box.Width > 1+epsilon || box.Y+box.Height > 1+epsilon {
		return fmt.Errorf("region exceeds image bounds")
	}
	return nil
}

func encodeCrop(data []byte, mimeType string, box normalizedBox) ([]byte, int, int, error) {
	var src image.Image
	var err error
	switch mimeType {
	case "image/png":
		src, err = png.Decode(bytes.NewReader(data))
	case "image/jpeg":
		src, err = jpeg.Decode(bytes.NewReader(data))
	default:
		return nil, 0, 0, fmt.Errorf("crop is not supported for %s", mimeType)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	bounds := src.Bounds()
	x0 := int(math.Round(box.X * float64(bounds.Dx())))
	y0 := int(math.Round(box.Y * float64(bounds.Dy())))
	x1 := int(math.Round((box.X + box.Width) * float64(bounds.Dx())))
	y1 := int(math.Round((box.Y + box.Height) * float64(bounds.Dy())))
	x0 = max(0, min(x0, bounds.Dx()))
	y0 = max(0, min(y0, bounds.Dy()))
	x1 = max(0, min(x1, bounds.Dx()))
	y1 = max(0, min(y1, bounds.Dy()))
	if x1 <= x0 || y1 <= y0 {
		return nil, 0, 0, fmt.Errorf("crop has zero pixels")
	}
	rect := image.Rect(x0, y0, x1, y1).Add(bounds.Min)
	dst := image.NewRGBA(image.Rect(0, 0, x1-x0, y1-y0))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, 0, 0, err
	}
	return out.Bytes(), x1 - x0, y1 - y0, nil
}

func writeControlledImage(root, assetID string, data []byte) (string, string, error) {
	rootCanonical, err := workspace.CanonicalPath(root)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(root, ".codeagent", "assets", "view-image")
	switch workspace.ClassifyPath(root, dir) {
	case workspace.PathInsideWorkspace:
	default:
		return "", "", fmt.Errorf("view_image crop directory escapes the workspace")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	dir, err = workspace.CanonicalPath(dir)
	if err != nil {
		return "", "", err
	}
	if workspace.ClassifyPath(rootCanonical, dir) != workspace.PathInsideWorkspace {
		return "", "", fmt.Errorf("view_image crop directory escapes the workspace through a symlink")
	}
	target := filepath.Join(dir, assetID+".png")
	if existing, err := os.ReadFile(target); err == nil {
		if !bytes.Equal(existing, data) {
			return "", "", fmt.Errorf("view_image crop asset already exists with different bytes")
		}
	} else if !os.IsNotExist(err) {
		return "", "", err
	} else {
		tmp, err := os.CreateTemp(dir, ".crop-*")
		if err != nil {
			return "", "", err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if err := tmp.Chmod(0o600); err != nil {
			_ = tmp.Close()
			return "", "", err
		}
		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			return "", "", err
		}
		if err := tmp.Close(); err != nil {
			return "", "", err
		}
		if err := os.Rename(tmpName, target); err != nil {
			return "", "", err
		}
	}
	rel, err := filepath.Rel(rootCanonical, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("view_image crop asset escapes the workspace")
	}
	return target, filepath.ToSlash(rel), nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func failureFromError(err error) tools.ToolResult {
	var codedErr *codedError
	if errors.As(err, &codedErr) {
		return tools.FailureResult(codedErr.kind, codedErr.message)
	}
	return tools.FailureResult("runtime", err.Error())
}
