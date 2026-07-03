package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"code-agent/internal/tools"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file implements the MCP *resources* primitive (R1): passive, read-only
// data a server exposes (files, schemas, docs). Unlike tools — which the model
// calls to act — resources are context the model pulls in to read. We surface
// them as two synthesized, read-only tools per server that declares the
// capability, so the model can discover and fetch them without any new frontend
// UI, and every frontend (CLI/TUI/wire) gets them for free:
//
//	mcp__<server>__list_resources   — enumerate resource URIs
//	mcp__<server>__read_resource    — fetch one resource's contents by URI
//
// Both are read-only: they do NOT implement tools.SideEffecting, so the loop runs
// them without an approval prompt and a read-only subagent may use them. (The
// MCP *prompts* primitive — user-invoked templates — remains out of scope here.)

// resourceCaller is the slice of *mcpsdk.ClientSession the resource tools need.
// Depending on the interface (not the concrete session) keeps them unit-testable
// with a fake — no subprocess required.
type resourceCaller interface {
	ListResources(ctx context.Context, params *mcpsdk.ListResourcesParams) (*mcpsdk.ListResourcesResult, error)
	ReadResource(ctx context.Context, params *mcpsdk.ReadResourceParams) (*mcpsdk.ReadResourceResult, error)
}

// newResourceTools returns the read-only list/read tools for one server. Called
// only when the server declares the resources capability (see Manager.connectOne).
func newResourceTools(caller resourceCaller, server string, log io.Writer) []tools.Tool {
	if log == nil {
		log = io.Discard
	}
	return []tools.Tool{
		&resourceListTool{caller: caller, server: server, log: log},
		&resourceReadTool{caller: caller, server: server, log: log},
	}
}

// --- list_resources -------------------------------------------------------

type resourceListTool struct {
	caller resourceCaller
	server string
	log    io.Writer
}

func (t *resourceListTool) Name() string         { return wireName(t.server, "list_resources") }
func (t *resourceListTool) displayLabel() string { return label(t.server, "list_resources") }

func (t *resourceListTool) Description() string {
	return fmt.Sprintf("List the readable resources exposed by the %q MCP server. "+
		"Returns each resource's URI (pass it to %s), name, MIME type, and description. "+
		"Resources are passive read-only context (files, schemas, docs), not actions.",
		t.server, wireName(t.server, "read_resource"))
}

func (t *resourceListTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

func (t *resourceListTool) Execute(ctx context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	fmt.Fprintf(t.log, "[mcp] call %s\n", label(t.server, "list_resources"))

	var all []*mcpsdk.Resource
	params := &mcpsdk.ListResourcesParams{}
	for {
		res, err := t.caller.ListResources(ctx, params)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("mcp: protocol error: %s: %w", label(t.server, "list_resources"), err)
		}
		all = append(all, res.Resources...)
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}

	if len(all) == 0 {
		return tools.ToolResult{Content: "(no resources)"}, nil
	}
	var b strings.Builder
	for _, r := range all {
		fmt.Fprintf(&b, "- %s", r.URI)
		if r.MIMEType != "" {
			fmt.Fprintf(&b, " (%s)", r.MIMEType)
		}
		if name := displayName(r); name != "" {
			fmt.Fprintf(&b, " — %s", name)
		}
		if r.Description != "" {
			fmt.Fprintf(&b, ": %s", r.Description)
		}
		b.WriteByte('\n')
	}
	return tools.ToolResult{Content: strings.TrimRight(b.String(), "\n")}, nil
}

// displayName prefers the human-facing Title, falling back to Name.
func displayName(r *mcpsdk.Resource) string {
	if r.Title != "" {
		return r.Title
	}
	return r.Name
}

// --- read_resource --------------------------------------------------------

type resourceReadTool struct {
	caller resourceCaller
	server string
	log    io.Writer
}

func (t *resourceReadTool) Name() string         { return wireName(t.server, "read_resource") }
func (t *resourceReadTool) displayLabel() string { return label(t.server, "read_resource") }

func (t *resourceReadTool) Description() string {
	return fmt.Sprintf("Read the contents of a resource from the %q MCP server by its URI "+
		"(get URIs from %s). Returns the resource's text; binary contents are noted but not inlined.",
		t.server, wireName(t.server, "list_resources"))
}

func (t *resourceReadTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string","description":"The resource URI to read, e.g. file:///path/to/doc or scheme://resource/path"}},"required":["uri"]}`)
}

func (t *resourceReadTool) Execute(ctx context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(input), &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("mcp: invalid arguments: %s: %w", label(t.server, "read_resource"), err)
	}
	if in.URI == "" {
		return tools.ToolResult{}, fmt.Errorf("mcp: invalid arguments: %s: \"uri\" is required", label(t.server, "read_resource"))
	}
	fmt.Fprintf(t.log, "[mcp] call %s uri=%s\n", label(t.server, "read_resource"), in.URI)

	res, err := t.caller.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: in.URI})
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("mcp: protocol error: %s: %w", label(t.server, "read_resource"), err)
	}
	return tools.ToolResult{Content: renderResourceContents(res.Contents)}, nil
}

// renderResourceContents concatenates a read result's parts: text inline, binary
// parts as a one-line placeholder (blobs are not inlined into the model context).
func renderResourceContents(contents []*mcpsdk.ResourceContents) string {
	if len(contents) == 0 {
		return "(empty resource)"
	}
	var parts []string
	for _, c := range contents {
		if c.Text != "" || len(c.Blob) == 0 {
			parts = append(parts, c.Text)
			continue
		}
		mime := c.MIMEType
		if mime == "" {
			mime = "application/octet-stream"
		}
		parts = append(parts, fmt.Sprintf("[binary content %s (%s, %d bytes) — not shown]", c.URI, mime, len(c.Blob)))
	}
	return strings.Join(parts, "\n")
}
