package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFlattenTextOnly(t *testing.T) {
	got := flattenContent([]mcpsdk.Content{
		&mcpsdk.TextContent{Text: "line one"},
		&mcpsdk.TextContent{Text: "line two"},
	})
	if got != "line one\nline two" {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenEmpty(t *testing.T) {
	if got := flattenContent(nil); got != "" {
		t.Fatalf("empty content should render empty, got %q", got)
	}
}

func TestFlattenNonTextPlaceholders(t *testing.T) {
	got := flattenContent([]mcpsdk.Content{
		&mcpsdk.TextContent{Text: "before"},
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("12345")},
		&mcpsdk.AudioContent{MIMEType: "audio/wav", Data: []byte("ab")},
		&mcpsdk.ResourceLink{URI: "file:///x"},
		&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{URI: "file:///y"}},
	})
	// Text survives; each non-text block becomes a labeled placeholder line.
	for _, want := range []string{
		"before",
		"[non-text content: image (image/png, 5 bytes) omitted]",
		"[non-text content: audio (audio/wav, 2 bytes) omitted]",
		"[non-text content: resource link file:///x omitted]",
		"[non-text content: embedded resource file:///y omitted]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("flattened output missing %q\ngot:\n%s", want, got)
		}
	}
}

func TestRenderNonTextAssets(t *testing.T) {
	got := renderContentAssets([]mcpsdk.Content{
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("12345")},
		&mcpsdk.ResourceLink{URI: "file:///tmp/report.html"},
	}, contentAssetContext{
		Server:        "fs",
		Tool:          "read_file",
		WorkspaceRoot: "/Users/x/project",
		TurnID:        "turn_1",
		CallID:        "call_2",
	})
	if len(got.Assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(got.Assets))
	}
	if got.Assets[0].Kind != "image" || got.Assets[0].MIMEType != "image/png" || got.Assets[0].Metadata["bytes"] != "5" {
		t.Fatalf("image asset = %+v", got.Assets[0])
	}
	if got.Assets[1].Kind != "url" || got.Assets[1].URI != "file:///tmp/report.html" {
		t.Fatalf("resource asset = %+v", got.Assets[1])
	}
	var out struct {
		Kind  string `json:"kind"`
		Items []struct {
			AssetID     string `json:"asset_id"`
			ContentKind string `json:"content_kind"`
		} `json:"items"`
	}
	if err := json.Unmarshal(got.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "mcp_content" || len(out.Items) != 2 || out.Items[0].AssetID != got.Assets[0].ID {
		t.Fatalf("output = %+v, assets = %+v", out, got.Assets)
	}
}

func TestRenderInlineImageMaterializesBlob(t *testing.T) {
	root := t.TempDir()
	data := []byte{0x89, 'P', 'N', 'G'}
	got := renderContentAssets([]mcpsdk.Content{
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: data},
	}, contentAssetContext{
		Server:        "desktop_control",
		Tool:          "screenshot_capture",
		WorkspaceRoot: root,
		TurnID:        "turn_1",
		CallID:        "call_2",
	})
	if len(got.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(got.Assets))
	}
	ref := got.Assets[0]
	if ref.Kind != "image" || ref.MIMEType != "image/png" {
		t.Fatalf("asset = %+v", ref)
	}
	if ref.WorkspaceRelativePath == "" || ref.AbsolutePath == "" || ref.Metadata["materialized"] != "true" {
		t.Fatalf("asset was not materialized: %+v", ref)
	}
	if ref.Metadata["sha256"] == "" {
		t.Fatalf("materialized asset is missing sha256: %+v", ref.Metadata)
	}
	if !strings.HasPrefix(filepath.ToSlash(ref.WorkspaceRelativePath), ".codeagent/assets/mcp/turn_1/call_2/") {
		t.Fatalf("workspace_relative_path = %q", ref.WorkspaceRelativePath)
	}
	onDisk, err := os.ReadFile(ref.AbsolutePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(data) {
		t.Fatalf("materialized bytes = %v, want %v", onDisk, data)
	}
}

func TestDesktopControlResourceLinkAliasesMaterializedImage(t *testing.T) {
	root := t.TempDir()
	artifactURI := "desktop-control://artifacts/artifact_123"
	got := renderContentAssets([]mcpsdk.Content{
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("png-bytes")},
		&mcpsdk.ResourceLink{URI: artifactURI},
	}, contentAssetContext{
		Server:        "desktop_control",
		Tool:          "screenshot_capture",
		WorkspaceRoot: root,
		TurnID:        "turn_1",
		CallID:        "call_2",
	})
	if len(got.Assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(got.Assets))
	}
	image := got.Assets[0]
	link := got.Assets[1]
	if image.Kind != "image" || image.WorkspaceRelativePath == "" {
		t.Fatalf("image asset = %+v", image)
	}
	if link.Kind != "image" || link.URI != artifactURI || link.MIMEType != "image/png" {
		t.Fatalf("resource link alias = %+v", link)
	}
	if link.WorkspaceRelativePath != image.WorkspaceRelativePath || link.AbsolutePath != image.AbsolutePath {
		t.Fatalf("resource link did not reuse materialized blob: image=%+v link=%+v", image, link)
	}
	if link.Metadata["alias_of"] != image.ID || link.Metadata["materialized"] != "true" {
		t.Fatalf("resource link alias metadata = %+v", link.Metadata)
	}
}

func TestFlattenNilEmbeddedResource(t *testing.T) {
	// A nil Resource pointer must not panic.
	got := flattenContent([]mcpsdk.Content{&mcpsdk.EmbeddedResource{}})
	if !strings.Contains(got, "embedded resource") {
		t.Fatalf("got %q", got)
	}
}

func TestMarshalSchemaFallback(t *testing.T) {
	if got := string(marshalSchema(nil)); got != `{"type":"object"}` {
		t.Fatalf("nil schema fallback = %s", got)
	}
	// A decoded JSON value (as the SDK delivers it) round-trips to raw bytes.
	got := string(marshalSchema(map[string]any{"type": "object"}))
	if !strings.Contains(got, `"type":"object"`) {
		t.Fatalf("schema passthrough = %s", got)
	}
}
