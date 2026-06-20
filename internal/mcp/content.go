package mcp

import (
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// flattenContent renders an MCP tool result's content blocks into a single
// string, because the agent loop's ToolResult is text-only. Text blocks are
// concatenated; non-text blocks (image/audio/resource) are replaced by a
// one-line placeholder so the model knows something was returned without the
// runtime pretending it can carry a binary payload.
//
// This is a deliberate v1 simplification, not a dead end: the raw blocks are not
// destroyed by anything here, and true multimodal passthrough is a coordinated
// model-layer change (model.Message.Content is itself a plain string today), out
// of scope for the MCP adapter.
func flattenContent(content []mcpsdk.Content) string {
	parts := make([]string, 0, len(content))
	for _, c := range content {
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			parts = append(parts, v.Text)
		case *mcpsdk.ImageContent:
			parts = append(parts, placeholder("image", v.MIMEType, len(v.Data)))
		case *mcpsdk.AudioContent:
			parts = append(parts, placeholder("audio", v.MIMEType, len(v.Data)))
		case *mcpsdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[non-text content: resource link %s omitted]", v.URI))
		case *mcpsdk.EmbeddedResource:
			uri := ""
			if v.Resource != nil {
				uri = v.Resource.URI
			}
			parts = append(parts, fmt.Sprintf("[non-text content: embedded resource %s omitted]", uri))
		default:
			parts = append(parts, "[non-text content omitted]")
		}
	}
	return strings.Join(parts, "\n")
}

func placeholder(kind, mime string, n int) string {
	if mime == "" {
		mime = "unknown"
	}
	return fmt.Sprintf("[non-text content: %s (%s, %d bytes) omitted]", kind, mime, n)
}
