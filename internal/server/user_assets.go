package server

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"code-agent/internal/model"
)

const maxUserAssetsPerTurn = 4
const maxLocalAssetsPerTurn = 16

var userAssetSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var localAssetID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// AgentInputRejected is the non-persisted v1.5 control response used before a
// turn exists. It deliberately carries no turn_id.
type AgentInputRejected struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Error     AgentInputError `json:"error"`
}

type AgentInputError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func rejectInput(requestID, code, message string) *AgentInputRejected {
	return &AgentInputRejected{
		Type: "agent_input_rejected", RequestID: requestID,
		Error: AgentInputError{Code: code, Message: message},
	}
}

// InputRejectionSink is transport-neutral. WebSocket, an in-process bridge, or
// a future HTTP command plane can each deliver the same control frame.
type InputRejectionSink interface {
	RejectInput(AgentInputRejected)
}

// decodeAndValidateAgentInput preserves raw asset object keys long enough to
// reject fields that are explicitly forbidden by v1.5, while continuing to
// ignore ordinary unknown fields for forward compatibility.
func decodeAndValidateAgentInput(data []byte, imageInput bool) (AgentInput, *AgentInputRejected) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return AgentInput{}, rejectInput("", "invalid_input", "invalid agent_input envelope")
	}
	requestID := rawString(raw["request_id"])
	var input AgentInput
	if err := json.Unmarshal(data, &input); err != nil {
		return AgentInput{}, rejectInput(requestID, "invalid_input", "invalid agent_input envelope")
	}

	assetsRaw, hasAssets := raw["assets"]
	localAssetsRaw, hasLocalAssets := raw["local_assets"]
	if (hasAssets || hasLocalAssets) && input.Kind != "text" {
		return AgentInput{}, rejectInput(requestID, "invalid_assets", "assets are only allowed on text input")
	}
	if input.Kind != "text" {
		return input, nil
	}
	if strings.TrimSpace(input.Text) == "" && len(input.Assets) == 0 && len(input.LocalAssets) == 0 {
		return AgentInput{}, rejectInput(requestID, "invalid_input", "text or attachments are required")
	}
	if len(input.Assets) > maxUserAssetsPerTurn {
		return AgentInput{}, rejectInput(requestID, "too_many_assets", "at most 4 image assets are allowed")
	}
	if len(input.LocalAssets) > maxLocalAssetsPerTurn {
		return AgentInput{}, rejectInput(requestID, "too_many_local_assets", "at most 16 local assets are allowed")
	}
	if (len(input.Assets) > 0 || len(input.LocalAssets) > 0) && strings.TrimSpace(input.RequestID) == "" {
		return AgentInput{}, rejectInput(requestID, "invalid_input", "request_id is required when assets are present")
	}
	if len(input.Assets) > 0 && !imageInput {
		return AgentInput{}, rejectInput(requestID, "image_input_unsupported", "image input is not available on this connection")
	}

	var assetObjects []map[string]json.RawMessage
	if len(input.Assets) > 0 {
		if err := json.Unmarshal(assetsRaw, &assetObjects); err != nil || len(assetObjects) != len(input.Assets) {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", "assets must be an array of objects")
		}
	}
	seen := make(map[int64]struct{}, len(input.Assets))
	for i, asset := range input.Assets {
		for _, key := range []string{"url", "oss_key", "object_key", "upload_id", "data", "bytes", "base64"} {
			if _, forbidden := assetObjects[i][key]; forbidden {
				return AgentInput{}, rejectInput(requestID, "invalid_assets", fmt.Sprintf("asset %d contains a forbidden field", asset.AssetID))
			}
		}
		if asset.AssetID <= 0 {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", "asset_id must be greater than zero")
		}
		if _, duplicate := seen[asset.AssetID]; duplicate {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", fmt.Sprintf("asset %d is duplicated", asset.AssetID))
		}
		seen[asset.AssetID] = struct{}{}
		if asset.Kind != "image" {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", fmt.Sprintf("asset %d has an unsupported kind", asset.AssetID))
		}
		if asset.MIMEType != "image/jpeg" && asset.MIMEType != "image/png" {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", fmt.Sprintf("asset %d has an unsupported mime_type", asset.AssetID))
		}
		if asset.SHA256 != "" && !userAssetSHA256.MatchString(asset.SHA256) {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", fmt.Sprintf("asset %d has an invalid sha256", asset.AssetID))
		}
		if !validUserAssetFilename(asset.Filename) {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", fmt.Sprintf("asset %d has an invalid filename", asset.AssetID))
		}
	}
	if len(input.LocalAssets) == 0 {
		return input, nil
	}
	// A mixed Gateway/local submission can be proven disjoint only when every
	// Gateway reference carries its content identity. Gateway-only submissions
	// retain backward compatibility with the optional SHA field.
	for _, asset := range input.Assets {
		if asset.SHA256 == "" {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", "sha256 is required for Gateway assets when local_assets are also present")
		}
	}
	var localObjects []map[string]json.RawMessage
	if err := json.Unmarshal(localAssetsRaw, &localObjects); err != nil || len(localObjects) != len(input.LocalAssets) {
		return AgentInput{}, rejectInput(requestID, "invalid_local_assets", "local_assets must be an array of objects")
	}
	seenIDs := make(map[string]struct{}, len(input.LocalAssets))
	seenPaths := make(map[string]struct{}, len(input.LocalAssets))
	gatewayHashes := make(map[string]struct{}, len(input.Assets))
	for _, asset := range input.Assets {
		if asset.SHA256 != "" {
			gatewayHashes[asset.SHA256] = struct{}{}
		}
	}
	for i, asset := range input.LocalAssets {
		allowed := map[string]struct{}{
			"id": {}, "relative_path": {}, "filename": {}, "mime_type": {},
			"kind": {}, "size_bytes": {}, "sha256": {}, "transfer_policy": {},
		}
		for key := range localObjects[i] {
			if _, known := allowed[key]; !known {
				return AgentInput{}, rejectInput(requestID, "invalid_local_assets", fmt.Sprintf("local asset %q contains a forbidden field", asset.ID))
			}
		}
		if !localAssetID.MatchString(asset.ID) {
			return AgentInput{}, rejectInput(requestID, "invalid_local_assets", "local asset has an invalid id")
		}
		if _, duplicate := seenIDs[asset.ID]; duplicate {
			return AgentInput{}, rejectInput(requestID, "invalid_local_assets", fmt.Sprintf("local asset id %q is duplicated", asset.ID))
		}
		seenIDs[asset.ID] = struct{}{}
		if !validLocalAssetPath(asset.RelativePath) {
			return AgentInput{}, rejectInput(requestID, "invalid_local_assets", fmt.Sprintf("local asset %q has an invalid relative_path", asset.ID))
		}
		if _, duplicate := seenPaths[asset.RelativePath]; duplicate {
			return AgentInput{}, rejectInput(requestID, "invalid_local_assets", fmt.Sprintf("local asset path %q is duplicated", asset.RelativePath))
		}
		seenPaths[asset.RelativePath] = struct{}{}
		if !validUserAssetFilename(asset.Filename) || asset.TransferPolicy != "local_only" ||
			asset.SizeBytes < 0 || !userAssetSHA256.MatchString(asset.SHA256) ||
			strings.TrimSpace(asset.MIMEType) == "" || strings.ContainsAny(asset.MIMEType, "\r\n\x00") ||
			strings.TrimSpace(asset.Kind) == "" || strings.ContainsAny(asset.Kind, "/\\\r\n\x00") {
			return AgentInput{}, rejectInput(requestID, "invalid_local_assets", fmt.Sprintf("local asset %q has invalid metadata", asset.ID))
		}
		if _, uploaded := gatewayHashes[asset.SHA256]; uploaded {
			return AgentInput{}, rejectInput(requestID, "invalid_assets", fmt.Sprintf("asset %q cannot be sent as both Gateway and local", asset.ID))
		}
	}
	return input, nil
}

func validLocalAssetPath(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\\\x00") || strings.Contains(value, "://") ||
		strings.HasPrefix(value, "/") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	segments := strings.Split(value, "/")
	if strings.Contains(segments[0], ":") {
		return false
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return path.Clean(value) == value
}

func validUserAssetFilename(filename string) bool {
	return filename != "" && len(filename) <= 255 && utf8.ValidString(filename) &&
		filename != "." && filename != ".." &&
		!strings.ContainsAny(filename, "/\\\x00") &&
		strings.IndexFunc(filename, unicode.IsControl) < 0
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func copyGatewayAssetRefs(in []model.GatewayAssetRef) []model.GatewayAssetRef {
	return append([]model.GatewayAssetRef(nil), in...)
}

func copyLocalAssetRefs(in []model.LocalAssetRef) []model.LocalAssetRef {
	return append([]model.LocalAssetRef(nil), in...)
}
