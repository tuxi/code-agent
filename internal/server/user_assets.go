package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"code-agent/internal/model"
)

const maxUserAssetsPerTurn = 4

var userAssetSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	if hasAssets && input.Kind != "text" {
		return AgentInput{}, rejectInput(requestID, "invalid_assets", "assets are only allowed on text input")
	}
	if input.Kind != "text" {
		return input, nil
	}
	if strings.TrimSpace(input.Text) == "" && len(input.Assets) == 0 {
		return AgentInput{}, rejectInput(requestID, "invalid_input", "text or assets are required")
	}
	if len(input.Assets) > maxUserAssetsPerTurn {
		return AgentInput{}, rejectInput(requestID, "too_many_assets", "at most 4 image assets are allowed")
	}
	if len(input.Assets) == 0 {
		return input, nil
	}
	if strings.TrimSpace(input.RequestID) == "" {
		return AgentInput{}, rejectInput(requestID, "invalid_input", "request_id is required when assets are present")
	}
	if !imageInput {
		return AgentInput{}, rejectInput(requestID, "image_input_unsupported", "image input is not available on this connection")
	}

	var assetObjects []map[string]json.RawMessage
	if err := json.Unmarshal(assetsRaw, &assetObjects); err != nil || len(assetObjects) != len(input.Assets) {
		return AgentInput{}, rejectInput(requestID, "invalid_assets", "assets must be an array of objects")
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
	return input, nil
}

func validUserAssetFilename(filename string) bool {
	return filename != "" && len(filename) <= 255 && utf8.ValidString(filename) &&
		filename != "." && filename != ".." &&
		!strings.ContainsAny(filename, "/\\\x00")
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func copyGatewayAssetRefs(in []model.GatewayAssetRef) []model.GatewayAssetRef {
	return append([]model.GatewayAssetRef(nil), in...)
}
