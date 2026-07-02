package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"code-agent/internal/assetref"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const internalOutputKindMCPContent = "mcp_content"

type contentAssetContext struct {
	Server        string
	Tool          string
	WorkspaceRoot string
	TurnID        string
	CallID        string
}

type renderedContent struct {
	Text   string
	Output json.RawMessage
	Assets []assets.Ref
}

// derivedMCPContentOutput is a code-agent Event.output shape derived from
// standard MCP content blocks. It is not sent to MCP servers, and it is not MCP
// structuredContent.
type derivedMCPContentOutput struct {
	Kind  string                        `json:"kind"`
	Items []derivedMCPContentOutputItem `json:"items"`
}

type derivedMCPContentOutputItem struct {
	AssetID     string `json:"asset_id"`
	Kind        string `json:"kind"`
	URI         string `json:"uri,omitempty"`
	MIMEType    string `json:"mime_type,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	ContentKind string `json:"content_kind,omitempty"`
}

// flattenContent renders an MCP tool result's content blocks into a single
// string. Text blocks are concatenated; non-text blocks (image/audio/resource)
// are replaced by a one-line placeholder so the model knows something was
// returned without the runtime pretending it can carry a binary payload.
//
// The richer side-channel lives in renderContentAssets, which preserves
// click/preview metadata for clients without changing the model transcript.
func flattenContent(content []mcpsdk.Content) string {
	return renderContentAssets(content, contentAssetContext{}).Text
}

func renderContentAssets(content []mcpsdk.Content, ctx contentAssetContext) renderedContent {
	parts := make([]string, 0, len(content))
	refs := make([]assets.Ref, 0)
	items := make([]derivedMCPContentOutputItem, 0)
	for i, c := range content {
		ordinal := i + 1
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			parts = append(parts, v.Text)
		case *mcpsdk.ImageContent:
			text := placeholder("image", v.MIMEType, len(v.Data))
			parts = append(parts, text)
			ref := nonTextAsset(ctx, ordinal, "image", "image", mcpContentURI(ctx, ordinal), v.MIMEType, len(v.Data), text)
			refs = append(refs, ref)
			items = append(items, outputItem(ref, "image", len(v.Data)))
		case *mcpsdk.AudioContent:
			text := placeholder("audio", v.MIMEType, len(v.Data))
			parts = append(parts, text)
			ref := nonTextAsset(ctx, ordinal, "audio", "audio", mcpContentURI(ctx, ordinal), v.MIMEType, len(v.Data), text)
			refs = append(refs, ref)
			items = append(items, outputItem(ref, "audio", len(v.Data)))
		case *mcpsdk.ResourceLink:
			text := fmt.Sprintf("[non-text content: resource link %s omitted]", v.URI)
			parts = append(parts, text)
			ref := nonTextAsset(ctx, ordinal, "url", "resource_link", v.URI, "", 0, text)
			refs = append(refs, ref)
			items = append(items, outputItem(ref, "resource_link", 0))
		case *mcpsdk.EmbeddedResource:
			uri := ""
			if v.Resource != nil {
				uri = v.Resource.URI
			}
			text := fmt.Sprintf("[non-text content: embedded resource %s omitted]", uri)
			parts = append(parts, text)
			ref := nonTextAsset(ctx, ordinal, "url", "embedded_resource", uri, "", 0, text)
			refs = append(refs, ref)
			items = append(items, outputItem(ref, "embedded_resource", 0))
		default:
			text := "[non-text content omitted]"
			parts = append(parts, text)
			ref := nonTextAsset(ctx, ordinal, "url", "unknown", mcpContentURI(ctx, ordinal), "", 0, text)
			refs = append(refs, ref)
			items = append(items, outputItem(ref, "unknown", 0))
		}
	}
	var output json.RawMessage
	if len(items) > 0 {
		output, _ = json.Marshal(derivedMCPContentOutput{Kind: internalOutputKindMCPContent, Items: items})
	}
	return renderedContent{Text: strings.Join(parts, "\n"), Output: output, Assets: refs}
}

func placeholder(kind, mime string, n int) string {
	if mime == "" {
		mime = "unknown"
	}
	return fmt.Sprintf("[non-text content: %s (%s, %d bytes) omitted]", kind, mime, n)
}

func nonTextAsset(ctx contentAssetContext, ordinal int, kind, contentKind, uri, mime string, bytes int, preview string) assets.Ref {
	if uri == "" {
		uri = mcpContentURI(ctx, ordinal)
	}
	id := assets.StableID(ctx.TurnID, ctx.CallID, ordinal, "mcp", contentKind, uri, mime, fmt.Sprint(bytes))
	meta := map[string]string{
		"source":       "mcp",
		"content_kind": contentKind,
		"mcp_type":     contentKind,
	}
	if ctx.Server != "" {
		meta["server"] = ctx.Server
	}
	if ctx.Tool != "" {
		meta["tool"] = ctx.Tool
	}
	if bytes > 0 {
		meta["bytes"] = fmt.Sprint(bytes)
	}
	ref := assets.Ref{
		ID:           id,
		Kind:         kind,
		URI:          uri,
		DisplayName:  displayNameForMCPAsset(kind, contentKind, uri, ordinal),
		Preview:      preview,
		MIMEType:     mime,
		Metadata:     meta,
		SourceTurnID: ctx.TurnID,
		SourceCallID: ctx.CallID,
	}
	if ctx.WorkspaceRoot != "" {
		ref.WorkspaceID = assets.WorkspaceID(ctx.WorkspaceRoot)
	}
	return ref
}

func outputItem(ref assets.Ref, contentKind string, bytes int) derivedMCPContentOutputItem {
	return derivedMCPContentOutputItem{
		AssetID:     ref.ID,
		Kind:        ref.Kind,
		URI:         ref.URI,
		MIMEType:    ref.MIMEType,
		Bytes:       bytes,
		ContentKind: contentKind,
	}
}

func mcpContentURI(ctx contentAssetContext, ordinal int) string {
	server := ctx.Server
	if server == "" {
		server = "unknown"
	}
	tool := ctx.Tool
	if tool == "" {
		tool = "unknown"
	}
	callID := ctx.CallID
	if callID == "" {
		callID = "unknown"
	}
	return fmt.Sprintf("mcp://%s/%s/%s/%03d", url.PathEscape(server), url.PathEscape(tool), url.PathEscape(callID), ordinal)
}

func displayNameForMCPAsset(kind, contentKind, uri string, ordinal int) string {
	if uri != "" {
		if u, err := url.Parse(uri); err == nil && u.Path != "" {
			if base := path.Base(u.Path); base != "." && base != "/" {
				return base
			}
		}
	}
	if contentKind != "" && contentKind != "unknown" {
		return fmt.Sprintf("%s %d", strings.ReplaceAll(contentKind, "_", " "), ordinal)
	}
	return fmt.Sprintf("%s %d", kind, ordinal)
}
