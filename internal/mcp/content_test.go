package mcp

import (
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
