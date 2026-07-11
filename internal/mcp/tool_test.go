package mcp

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeCaller is a stand-in for *mcpsdk.ClientSession so remoteTool can be tested
// without spawning a subprocess.
type fakeCaller struct {
	gotParams *mcpsdk.CallToolParams
	res       *mcpsdk.CallToolResult
	err       error
	readBy    map[string]*mcpsdk.ReadResourceResult
	readErr   error
}

func (f *fakeCaller) CallTool(_ context.Context, p *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	f.gotParams = p
	return f.res, f.err
}

func (f *fakeCaller) ReadResource(_ context.Context, p *mcpsdk.ReadResourceParams) (*mcpsdk.ReadResourceResult, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.readBy == nil {
		return nil, errors.New("resource not found")
	}
	res := f.readBy[p.URI]
	if res == nil {
		return nil, errors.New("resource not found")
	}
	return res, nil
}

func newTestTool(c toolCaller) *remoteTool {
	return &remoteTool{
		caller:     c,
		server:     "fs",
		remoteName: "read_file",
		wireName:   wireName("fs", "read_file"),
		label:      label("fs", "read_file"),
		log:        io.Discard,
	}
}

func textResult(text string, isErr bool) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		IsError: isErr,
	}
}

func TestExecuteSuccess(t *testing.T) {
	c := &fakeCaller{res: textResult("hello world", false)}
	got, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Content != "hello world" {
		t.Fatalf("content = %q, want %q", got.Content, "hello world")
	}
	// Arguments must be forwarded as raw JSON, not laundered through a map.
	raw, ok := c.gotParams.Arguments.(json.RawMessage)
	if !ok {
		t.Fatalf("Arguments type = %T, want json.RawMessage", c.gotParams.Arguments)
	}
	if string(raw) != `{"path":"a.txt"}` {
		t.Fatalf("forwarded args = %s, want raw passthrough", raw)
	}
	if c.gotParams.Name != "read_file" {
		t.Fatalf("called remote name %q, want read_file", c.gotParams.Name)
	}
}

func TestExecutePreservesNonTextAssets(t *testing.T) {
	root := t.TempDir()
	c := &fakeCaller{res: &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "before"},
			&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("12345")},
		},
	}}
	got, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_1",
		CallID:        "call_2",
	}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got.Content, "[non-text content: image (image/png, 5 bytes) omitted]") {
		t.Fatalf("content = %q, want image placeholder", got.Content)
	}
	if len(got.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(got.Assets))
	}
	if got.Assets[0].Kind != "image" || got.Assets[0].MIMEType != "image/png" || got.Assets[0].WorkspaceID == "" {
		t.Fatalf("asset = %+v", got.Assets[0])
	}
	if got.Assets[0].WorkspaceRelativePath == "" || got.Assets[0].AbsolutePath == "" || got.Assets[0].Metadata["materialized"] != "true" {
		t.Fatalf("asset was not materialized: %+v", got.Assets[0])
	}
	if len(got.Output) == 0 || !strings.Contains(string(got.Output), `"kind":"mcp_content"`) {
		t.Fatalf("output = %s, want mcp_content", got.Output)
	}
}

func TestExecuteMaterializesResourceLinkViaReadResource(t *testing.T) {
	root := t.TempDir()
	uri := "desktop-control://artifacts/artifact_123"
	c := &fakeCaller{
		res: &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{
				&mcpsdk.ResourceLink{URI: uri, MIMEType: "image/png"},
			},
		},
		readBy: map[string]*mcpsdk.ReadResourceResult{
			uri: {Contents: []*mcpsdk.ResourceContents{
				{URI: uri, MIMEType: "image/png", Blob: []byte("png-bytes")},
			}},
		},
	}
	got, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: root,
		TurnID:        "turn_1",
		CallID:        "call_2",
	}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(got.Assets))
	}
	ref := got.Assets[0]
	if ref.Kind != "image" || ref.URI != uri || ref.MIMEType != "image/png" {
		t.Fatalf("asset = %+v", ref)
	}
	if ref.WorkspaceRelativePath == "" || ref.AbsolutePath == "" || ref.Metadata["resource_read"] != "true" {
		t.Fatalf("resource link was not materialized: %+v", ref)
	}
	data, err := os.ReadFile(ref.AbsolutePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "png-bytes" {
		t.Fatalf("materialized bytes = %q", data)
	}
}

func TestExecuteEmptyInputSendsNoArguments(t *testing.T) {
	c := &fakeCaller{res: textResult("ok", false)}
	if _, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.gotParams.Arguments != nil {
		t.Fatalf("Arguments = %v, want nil for empty input", c.gotParams.Arguments)
	}
}

func TestExecuteInvalidArguments(t *testing.T) {
	c := &fakeCaller{res: textResult("unused", false)}
	_, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{not json`))
	if err == nil || !strings.HasPrefix(err.Error(), "mcp: invalid arguments:") {
		t.Fatalf("err = %v, want a 'mcp: invalid arguments' error", err)
	}
	if c.gotParams != nil {
		t.Fatalf("CallTool should not be reached on invalid input")
	}
}

func TestExecuteProtocolError(t *testing.T) {
	c := &fakeCaller{err: errors.New("connection reset")}
	_, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{}`))
	if err == nil || !strings.HasPrefix(err.Error(), "mcp: protocol error:") {
		t.Fatalf("err = %v, want a 'mcp: protocol error'", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v, should wrap the underlying cause", err)
	}
}

func TestExecuteToolError(t *testing.T) {
	c := &fakeCaller{res: textResult("file not found", true)}
	_, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{}`))
	if err == nil || !strings.HasPrefix(err.Error(), "mcp: tool error:") {
		t.Fatalf("err = %v, want a 'mcp: tool error'", err)
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("err = %v, should carry the tool's content", err)
	}
}

func TestNameSplit(t *testing.T) {
	rt := newTestTool(&fakeCaller{})
	if rt.Name() != "mcp__fs__read_file" {
		t.Fatalf("wire name = %q, want mcp__fs__read_file", rt.Name())
	}
	if rt.label != "mcp.fs.read_file" {
		t.Fatalf("label = %q, want mcp.fs.read_file", rt.label)
	}
}

func TestWireNameSanitizesIllegalChars(t *testing.T) {
	// '.', ':' and '/' are illegal in provider tool names and must be replaced.
	got := wireName("my.server", "do:it/now")
	if strings.ContainsAny(got, ".:/") {
		t.Fatalf("wire name %q still contains illegal characters", got)
	}
	want := "mcp__my_server__do_it_now"
	if got != want {
		t.Fatalf("wire name = %q, want %q", got, want)
	}
}

func TestMergeOutputNilStructured(t *testing.T) {
	// When StructuredContent is nil, derived output is returned as-is.
	derived := json.RawMessage(`{"kind":"mcp_content","items":[]}`)
	got := mergeOutput(nil, derived)
	if string(got) != string(derived) {
		t.Fatalf("got %s, want derived passthrough", got)
	}
}

func TestMergeOutputNilStructNilDerived(t *testing.T) {
	got := mergeOutput(nil, nil)
	if got != nil {
		t.Fatalf("got %s, want nil", got)
	}
}

func TestMergeOutputStructOnly(t *testing.T) {
	// When only StructuredContent is present (no derived), it becomes Output.
	structured := map[string]any{
		"evidence": map[string]any{"auditEventID": "audit_001"},
	}
	got := mergeOutput(structured, nil)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("output not a JSON object: %s", got)
	}
	var evidence map[string]string
	if err := json.Unmarshal(m["evidence"], &evidence); err != nil {
		t.Fatalf("evidence key missing or wrong type: %s", got)
	}
	if evidence["auditEventID"] != "audit_001" {
		t.Fatalf("auditEventID = %q, want audit_001", evidence["auditEventID"])
	}
}

func TestMergeOutputBothPresent(t *testing.T) {
	// When both are present, derived items nest under _mcp_content.
	structured := map[string]any{
		"execution": map[string]any{"status": "ok"},
		"evidence":  map[string]any{"auditEventID": "audit_002"},
	}
	derived := json.RawMessage(`{"kind":"mcp_content","items":[{"kind":"image"}]}`)
	got := mergeOutput(structured, derived)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("output not a JSON object: %s", got)
	}
	// StructuredContent keys at top level.
	if _, ok := m["execution"]; !ok {
		t.Fatalf("execution key missing from output: %s", got)
	}
	if _, ok := m["evidence"]; !ok {
		t.Fatalf("evidence key missing from output: %s", got)
	}
	// Derived content nested.
	content, ok := m["_mcp_content"]
	if !ok {
		t.Fatalf("_mcp_content key missing from output: %s", got)
	}
	if !strings.Contains(string(content), "mcp_content") {
		t.Fatalf("_mcp_content = %s, want nested derived output", content)
	}
}

func TestMergeOutputWithDesktopControlShape(t *testing.T) {
	// Exact shape from desktop-control-mcp: empty content blocks,
	// structuredContent carries the evidence.
	structured := map[string]any{
		"execution":    map[string]any{},
		"verification": map[string]any{},
		"evidence": map[string]any{
			"auditEventID": "audit_dc_42",
		},
	}
	got := mergeOutput(structured, nil)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("output not a JSON object: %s", got)
	}
	var evidence map[string]string
	if err := json.Unmarshal(m["evidence"], &evidence); err != nil {
		t.Fatalf("evidence key missing or wrong type: %s", got)
	}
	if evidence["auditEventID"] != "audit_dc_42" {
		t.Fatalf("auditEventID = %q, want audit_dc_42", evidence["auditEventID"])
	}
}

func TestExecuteForwardsStructuredContent(t *testing.T) {
	// The full integration: remoteTool.Execute → ToolResult carries structuredContent
	// in Output, so AgentKit can read tool_finished.output.evidence.auditEventID.
	structured := map[string]any{
		"evidence": map[string]any{"auditEventID": "audit_int_1"},
	}
	c := &fakeCaller{res: &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "done"}},
		StructuredContent: structured,
	}}
	got, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Content != "done" {
		t.Fatalf("content = %q, want done", got.Content)
	}
	if len(got.Output) == 0 {
		t.Fatal("Output is empty; StructuredContent was dropped")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got.Output, &m); err != nil {
		t.Fatalf("Output is not a JSON object: %s", got.Output)
	}
	var evidence map[string]string
	if err := json.Unmarshal(m["evidence"], &evidence); err != nil {
		t.Fatalf("evidence key missing: %s", got.Output)
	}
	if evidence["auditEventID"] != "audit_int_1" {
		t.Fatalf("auditEventID = %q, want audit_int_1", evidence["auditEventID"])
	}
}

func TestExecuteStructuredContentAndNonTextAssets(t *testing.T) {
	// When both structuredContent and non-text blocks (image) are present,
	// structuredContent is at top level and derived items under _mcp_content.
	structured := map[string]any{
		"evidence": map[string]any{"auditEventID": "audit_both"},
	}
	c := &fakeCaller{res: &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "done with image"},
			&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("abc")},
		},
		StructuredContent: structured,
	}}
	got, err := newTestTool(c).Execute(context.Background(), tools.ExecutionContext{
		TurnID: "t1", CallID: "c1",
	}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got.Output, &m); err != nil {
		t.Fatalf("Output is not a JSON object: %s", got.Output)
	}
	// Structured content at top level.
	if _, ok := m["evidence"]; !ok {
		t.Fatalf("evidence key missing: %s", got.Output)
	}
	// Derived items still present.
	if _, ok := m["_mcp_content"]; !ok {
		t.Fatalf("_mcp_content key missing (non-text assets dropped): %s", got.Output)
	}
	// Assets still present.
	if len(got.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(got.Assets))
	}
}

func TestSideEffectsAlwaysTrue(t *testing.T) {
	if !newTestTool(&fakeCaller{}).SideEffects() {
		t.Fatal("remote tools must always declare side effects")
	}
}
