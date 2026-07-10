package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"code-agent/internal/tools"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolCaller is the narrow slice of *mcpsdk.ClientSession that remoteTool needs.
// Depending on the interface (not the concrete session) keeps remoteTool unit-
// testable with a fake — no subprocess required.
type toolCaller interface {
	CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error)
}

// remoteTool adapts one tool exposed by an MCP server into a tools.Tool, so it
// lives in the same Registry as built-in tools and is gated by the same policy
// layer. It carries the server and remote name as separate fields, so the wire
// name is never parsed back apart.
type remoteTool struct {
	caller     toolCaller
	server     string // configured server name
	remoteName string // the tool's name on the server (sent in tools/call)
	wireName   string // mcp__<server>__<tool> — advertised to the model (provider-safe charset)
	label      string // mcp.<server>.<tool> — human-readable, for the startup summary and debug trace
	desc       string
	schema     json.RawMessage // captured at list time, passed through untouched
	log        io.Writer       // raw I/O trace; never nil (set by the Manager)
}

// displayLabeled is implemented by our MCP-backed tools to expose the readable
// dotted label (mcp.<server>.<tool>) for the startup summary, so every registered
// MCP tool — remote tools and synthesized resource tools alike — is listed by
// name, not just remoteTool.
type displayLabeled interface{ displayLabel() string }

func (t *remoteTool) displayLabel() string { return t.label }

func (t *remoteTool) Name() string                 { return t.wireName }
func (t *remoteTool) Description() string          { return t.desc }
func (t *remoteTool) InputSchema() json.RawMessage { return t.schema }

// SideEffects marks every remote tool as side-effecting. We cannot know whether
// a given remote tool mutates state, so the safe default is to route every call
// through the Approver.
func (t *remoteTool) SideEffects() bool { return true }

// Execute forwards the call to the MCP server. It maps three distinct failure
// modes to distinct, classifiable prefixes so the model can tell a (possibly
// transient) infrastructure error from a semantic tool failure or a bad-argument
// error it should fix:
//
//	mcp: invalid arguments  — the model's JSON args are malformed
//	mcp: protocol error     — the call itself failed (transport, unknown tool, ...)
//	mcp: tool error         — the tool ran but reported IsError
func (t *remoteTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	fmt.Fprintf(t.log, "[mcp] call %s args=%s\n", t.label, rawForLog(input))

	params := &mcpsdk.CallToolParams{Name: t.remoteName}
	// Pass the model's arguments through as raw JSON (Arguments is `any`, and a
	// json.RawMessage marshals verbatim) so we never launder types through a
	// map[string]any. Empty input means "no arguments".
	if trimmed := bytes.TrimSpace(input); len(trimmed) > 0 {
		if !json.Valid(trimmed) {
			return tools.ToolResult{}, fmt.Errorf("mcp: invalid arguments: %s: not valid JSON", t.label)
		}
		params.Arguments = json.RawMessage(trimmed)
	}

	res, err := t.caller.CallTool(ctx, params)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("mcp: protocol error: %s: %w", t.label, err)
	}

	rendered := renderContentAssets(res.Content, contentAssetContext{
		Server:        t.server,
		Tool:          t.remoteName,
		WorkspaceRoot: ec.WorkspaceRoot,
		TurnID:        ec.TurnID,
		CallID:        ec.CallID,
	})
	text := rendered.Text
	fmt.Fprintf(t.log, "[mcp] result %s isError=%t bytes=%d\n", t.label, res.IsError, len(text))

	if res.IsError {
		// The tool ran but failed; its content is meant for the model to read and
		// self-correct, so surface it through the loop's existing failure path.
		return tools.ToolResult{}, fmt.Errorf("mcp: tool error: %s", text)
	}
	return tools.ToolResult{Content: text, Output: mergeOutput(res.StructuredContent, rendered.Output), Assets: rendered.Assets}, nil
}

// mergeOutput combines the MCP server's StructuredContent (the semantically
// meaningful structured result — e.g. execution, verification, evidence) with
// the content-block-derived output (non-text blocks like images/audio/resources)
// into a single json.RawMessage. StructuredContent sits at the top level so
// downstream consumers (AgentKit TimelineExtension) can access it directly via
// tool_finished.output; any derived items nest under _mcp_content so they never
// collide with structured-content keys.
func mergeOutput(structured any, derived json.RawMessage) json.RawMessage {
	if structured == nil {
		return derived
	}
	sc, err := json.Marshal(structured)
	if err != nil {
		return derived
	}
	if len(derived) == 0 {
		return json.RawMessage(sc)
	}
	// Both present: nest derived items under _mcp_content so the structured
	// result is flat and directly addressable.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(sc, &m); err != nil {
		// structuredContent is not a JSON object (array, string, number, …);
		// wrap both under explicit keys so nothing is dropped.
		merged := make(map[string]json.RawMessage)
		merged["value"] = json.RawMessage(sc)
		merged["_mcp_content"] = derived
		b, _ := json.Marshal(merged)
		return json.RawMessage(b)
	}
	m["_mcp_content"] = derived
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(sc)
	}
	return json.RawMessage(b)
}

func rawForLog(b []byte) string {
	if len(bytes.TrimSpace(b)) == 0 {
		return "(none)"
	}
	return string(b)
}

// unsafeNameChars matches everything NOT allowed in a model-facing tool name.
var unsafeNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// wireName is the provider-facing tool name. Function names must match
// ^[a-zA-Z0-9_-]+$ for OpenAI/Anthropic-style tool calling, so '.', ':' and '/'
// are illegal — which is exactly why the namespace on the wire uses double
// underscores (mcp__<server>__<tool>) and the dotted form is kept only for human
// display (see label).
func wireName(server, tool string) string {
	return "mcp__" + sanitize(server) + "__" + sanitize(tool)
}

// label is the human-readable name shown in the startup summary and the debug
// trace. It is never sent to a provider, so it can use the cleaner dotted form.
// (The loop's approval prompt receives the wire name, since that is the only
// name the model-facing call carries — surfacing the label there is a later
// nicety that would need the approver to resolve it.)
func label(server, tool string) string {
	return "mcp." + server + "." + tool
}

func sanitize(s string) string {
	return unsafeNameChars.ReplaceAllString(s, "_")
}

// marshalSchema converts a remote tool's input schema (delivered by the SDK as a
// decoded JSON value) into the raw bytes our Tool interface advertises. The
// tools package explicitly allows returning a raw schema, so it is passed
// through unmodified. A missing or unmarshalable schema falls back to an empty
// object schema, so the model still sees a well-formed parameters field.
func marshalSchema(schema any) json.RawMessage {
	if schema == nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	b, err := json.Marshal(schema)
	if err != nil || len(b) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}
