package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"code-agent/internal/assetref"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const internalOutputKindMCPContent = "mcp_content"
const maxMaterializedMCPAssetBytes = 50 * 1024 * 1024

type resourceReadCaller interface {
	ReadResource(ctx context.Context, params *mcpsdk.ReadResourceParams) (*mcpsdk.ReadResourceResult, error)
}

type contentAssetContext struct {
	Server         string
	Tool           string
	WorkspaceRoot  string
	TurnID         string
	CallID         string
	Context        context.Context
	ResourceReader resourceReadCaller
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
	var lastMaterializedMedia *assets.Ref
	for i, c := range content {
		ordinal := i + 1
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			parts = append(parts, v.Text)
		case *mcpsdk.ImageContent:
			text := placeholder("image", v.MIMEType, len(v.Data))
			parts = append(parts, text)
			ref := nonTextAsset(ctx, ordinal, "image", "image", mcpContentURI(ctx, ordinal), v.MIMEType, len(v.Data), text)
			materializeInlineAsset(ctx, &ref, v.Data)
			refs = append(refs, ref)
			items = append(items, outputItem(ref, "image", len(v.Data)))
			if ref.WorkspaceRelativePath != "" {
				lastMaterializedMedia = &refs[len(refs)-1]
			}
		case *mcpsdk.AudioContent:
			text := placeholder("audio", v.MIMEType, len(v.Data))
			parts = append(parts, text)
			ref := nonTextAsset(ctx, ordinal, "audio", "audio", mcpContentURI(ctx, ordinal), v.MIMEType, len(v.Data), text)
			materializeInlineAsset(ctx, &ref, v.Data)
			refs = append(refs, ref)
			items = append(items, outputItem(ref, "audio", len(v.Data)))
			if ref.WorkspaceRelativePath != "" {
				lastMaterializedMedia = &refs[len(refs)-1]
			}
		case *mcpsdk.ResourceLink:
			text := fmt.Sprintf("[non-text content: resource link %s omitted]", v.URI)
			parts = append(parts, text)
			ref := nonTextAsset(ctx, ordinal, "url", "resource_link", v.URI, "", 0, text)
			bytes := 0
			if aliased, ok := aliasResourceLinkToMedia(ctx, ordinal, v.URI, text, lastMaterializedMedia); ok {
				ref = aliased
				bytes = metadataBytes(ref)
			} else if materialized, n, ok := materializeResourceLink(ctx, ordinal, v, text); ok {
				ref = materialized
				bytes = n
			}
			refs = append(refs, ref)
			items = append(items, outputItem(ref, "resource_link", bytes))
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

func aliasResourceLinkToMedia(ctx contentAssetContext, ordinal int, uri, preview string, media *assets.Ref) (assets.Ref, bool) {
	if media == nil || !isDesktopControlArtifactURI(uri) || media.WorkspaceRelativePath == "" {
		return assets.Ref{}, false
	}
	ref := nonTextAsset(ctx, ordinal, media.Kind, "resource_link", uri, media.MIMEType, 0, preview)
	ref.WorkspaceRelativePath = media.WorkspaceRelativePath
	ref.AbsolutePath = media.AbsolutePath
	ref.WorkspaceID = media.WorkspaceID
	ref.DisplayName = displayNameForMCPAsset(media.Kind, "resource_link", uri, ordinal)
	if ref.Metadata == nil {
		ref.Metadata = map[string]string{}
	}
	ref.Metadata["alias_of"] = media.ID
	ref.Metadata["materialized"] = media.Metadata["materialized"]
	ref.Metadata["materialized_path"] = media.Metadata["materialized_path"]
	if bytes := media.Metadata["bytes"]; bytes != "" {
		ref.Metadata["bytes"] = bytes
	}
	return ref, true
}

func isDesktopControlArtifactURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "desktop-control" && u.Host == "artifacts"
}

func materializeResourceLink(ctx contentAssetContext, ordinal int, link *mcpsdk.ResourceLink, preview string) (assets.Ref, int, bool) {
	if link == nil || link.URI == "" || ctx.ResourceReader == nil || ctx.WorkspaceRoot == "" {
		return assets.Ref{}, 0, false
	}
	callCtx := ctx.Context
	if callCtx == nil {
		callCtx = context.Background()
	}
	res, err := ctx.ResourceReader.ReadResource(callCtx, &mcpsdk.ReadResourceParams{URI: link.URI})
	if err != nil {
		return assets.Ref{}, 0, false
	}
	for _, content := range res.Contents {
		if content == nil || len(content.Blob) == 0 {
			continue
		}
		mimeType := content.MIMEType
		if mimeType == "" {
			mimeType = link.MIMEType
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		kind := assets.KindForMIME(mimeType)
		ref := nonTextAsset(ctx, ordinal, kind, "resource_link", link.URI, mimeType, len(content.Blob), preview)
		materializeInlineAsset(ctx, &ref, content.Blob)
		if ref.WorkspaceRelativePath == "" {
			return assets.Ref{}, 0, false
		}
		ref.Metadata["resource_read"] = "true"
		return ref, len(content.Blob), true
	}
	return assets.Ref{}, 0, false
}

func metadataBytes(ref assets.Ref) int {
	if ref.Metadata == nil {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(ref.Metadata["bytes"], "%d", &n)
	return n
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

func materializeInlineAsset(ctx contentAssetContext, ref *assets.Ref, data []byte) {
	if ref == nil || ctx.WorkspaceRoot == "" || len(data) == 0 || len(data) > maxMaterializedMCPAssetBytes {
		return
	}
	rel := filepath.ToSlash(filepath.Join(".codeagent", "assets", "mcp", safePathPart(ctx.TurnID), safePathPart(ctx.CallID), ref.ID+extensionForMIME(ref.MIMEType)))
	abs := filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return
	}
	ref.WorkspaceRelativePath = rel
	ref.AbsolutePath = abs
	if ref.Metadata == nil {
		ref.Metadata = map[string]string{}
	}
	ref.Metadata["materialized"] = "true"
	ref.Metadata["materialized_path"] = rel
}

func extensionForMIME(mimeType string) string {
	m := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch m {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/mpeg":
		return ".mp3"
	case "video/mp4":
		return ".mp4"
	}
	if exts, err := mime.ExtensionsByType(m); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

func safePathPart(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '=':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unknown"
	}
	return out
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
