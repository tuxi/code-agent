package mcp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"code-agent/internal/tools"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeResourceCaller serves canned list/read results and paginates once when
// pages has more than one entry, so the list tool's cursor loop is exercised.
type fakeResourceCaller struct {
	pages    [][]*mcpsdk.Resource // successive ListResources responses
	listErr  error
	readBy   map[string]*mcpsdk.ReadResourceResult
	readErr  error
	listCall int
}

func (f *fakeResourceCaller) ListResources(_ context.Context, _ *mcpsdk.ListResourcesParams) (*mcpsdk.ListResourcesResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	i := f.listCall
	f.listCall++
	res := &mcpsdk.ListResourcesResult{Resources: f.pages[i]}
	if i < len(f.pages)-1 {
		res.NextCursor = "more"
	}
	return res, nil
}

func (f *fakeResourceCaller) ReadResource(_ context.Context, p *mcpsdk.ReadResourceParams) (*mcpsdk.ReadResourceResult, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.readBy[p.URI], nil
}

func resourceTools(t *testing.T, f *fakeResourceCaller) (*resourceListTool, *resourceReadTool) {
	t.Helper()
	got := newResourceTools(f, "docs", io.Discard)
	if len(got) != 2 {
		t.Fatalf("expected 2 resource tools, got %d", len(got))
	}
	return got[0].(*resourceListTool), got[1].(*resourceReadTool)
}

func TestResourceToolNames(t *testing.T) {
	lst, rd := resourceTools(t, &fakeResourceCaller{})
	if lst.Name() != "mcp__docs__list_resources" {
		t.Errorf("list tool name = %q", lst.Name())
	}
	if rd.Name() != "mcp__docs__read_resource" {
		t.Errorf("read tool name = %q", rd.Name())
	}
}

// list_resources pages through all cursors and formats uri/mime/name/description.
func TestResourceListPaginatesAndFormats(t *testing.T) {
	f := &fakeResourceCaller{pages: [][]*mcpsdk.Resource{
		{{URI: "file:///a.md", MIMEType: "text/markdown", Title: "A", Description: "first"}},
		{{URI: "db://schema", Name: "schema"}},
	}}
	lst, _ := resourceTools(t, f)

	out, err := lst.Execute(context.Background(), tools.ExecutionContext{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.listCall != 2 {
		t.Fatalf("expected 2 ListResources calls (pagination), got %d", f.listCall)
	}
	if !strings.Contains(out.Content, "file:///a.md") || !strings.Contains(out.Content, "(text/markdown)") ||
		!strings.Contains(out.Content, "A") || !strings.Contains(out.Content, "first") {
		t.Fatalf("list output missing fields:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "db://schema") {
		t.Fatalf("second page missing:\n%s", out.Content)
	}
}

func TestResourceListEmpty(t *testing.T) {
	f := &fakeResourceCaller{pages: [][]*mcpsdk.Resource{{}}}
	lst, _ := resourceTools(t, f)
	out, err := lst.Execute(context.Background(), tools.ExecutionContext{}, nil)
	if err != nil || out.Content != "(no resources)" {
		t.Fatalf("empty list should say so, got %q err=%v", out.Content, err)
	}
}

// read_resource concatenates text contents and placeholders binary parts.
func TestResourceReadTextAndBlob(t *testing.T) {
	f := &fakeResourceCaller{readBy: map[string]*mcpsdk.ReadResourceResult{
		"file:///a.md": {Contents: []*mcpsdk.ResourceContents{
			{URI: "file:///a.md", MIMEType: "text/markdown", Text: "# Title\nbody"},
			{URI: "file:///a.png", MIMEType: "image/png", Blob: []byte{1, 2, 3, 4}},
		}},
	}}
	_, rd := resourceTools(t, f)

	out, err := rd.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"uri":"file:///a.md"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Content, "# Title\nbody") {
		t.Fatalf("text content missing:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "binary content file:///a.png (image/png, 4 bytes)") {
		t.Fatalf("blob placeholder missing:\n%s", out.Content)
	}
}

func TestResourceReadRequiresURI(t *testing.T) {
	_, rd := resourceTools(t, &fakeResourceCaller{})
	if _, err := rd.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{}`)); err == nil {
		t.Fatal("read without uri should error")
	}
}

// Resource tools are read-only: they must NOT implement tools.SideEffecting, so
// the loop runs them without an approval prompt.
func TestResourceToolsAreReadOnly(t *testing.T) {
	lst, rd := resourceTools(t, &fakeResourceCaller{})
	for _, tool := range []any{lst, rd} {
		if _, ok := tool.(interface{ SideEffects() bool }); ok {
			t.Fatalf("%T must be read-only (no SideEffects marker)", tool)
		}
	}
}
