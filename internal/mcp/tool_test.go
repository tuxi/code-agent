package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
}

func (f *fakeCaller) CallTool(_ context.Context, p *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	f.gotParams = p
	return f.res, f.err
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
	got, err := newTestTool(c).Execute(context.Background(), json.RawMessage(`{"path":"a.txt"}`))
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

func TestExecuteEmptyInputSendsNoArguments(t *testing.T) {
	c := &fakeCaller{res: textResult("ok", false)}
	if _, err := newTestTool(c).Execute(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.gotParams.Arguments != nil {
		t.Fatalf("Arguments = %v, want nil for empty input", c.gotParams.Arguments)
	}
}

func TestExecuteInvalidArguments(t *testing.T) {
	c := &fakeCaller{res: textResult("unused", false)}
	_, err := newTestTool(c).Execute(context.Background(), json.RawMessage(`{not json`))
	if err == nil || !strings.HasPrefix(err.Error(), "mcp: invalid arguments:") {
		t.Fatalf("err = %v, want a 'mcp: invalid arguments' error", err)
	}
	if c.gotParams != nil {
		t.Fatalf("CallTool should not be reached on invalid input")
	}
}

func TestExecuteProtocolError(t *testing.T) {
	c := &fakeCaller{err: errors.New("connection reset")}
	_, err := newTestTool(c).Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.HasPrefix(err.Error(), "mcp: protocol error:") {
		t.Fatalf("err = %v, want a 'mcp: protocol error'", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v, should wrap the underlying cause", err)
	}
}

func TestExecuteToolError(t *testing.T) {
	c := &fakeCaller{res: textResult("file not found", true)}
	_, err := newTestTool(c).Execute(context.Background(), json.RawMessage(`{}`))
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

func TestSideEffectsAlwaysTrue(t *testing.T) {
	if !newTestTool(&fakeCaller{}).SideEffects() {
		t.Fatal("remote tools must always declare side effects")
	}
}
