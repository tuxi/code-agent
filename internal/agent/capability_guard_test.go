package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
	"code-agent/internal/tools/imageview"
	"code-agent/internal/tools/uilayout"
)

// evidenceProbe simulates a client-side inspect_ui_image tool that still returns
// ui_evidence.v1 after view_image reported a missing tool-image capability.
type evidenceProbe struct{}

func (evidenceProbe) Name() string                 { return "inspect_ui_image" }
func (evidenceProbe) Description() string          { return "returns ui_evidence.v1" }
func (evidenceProbe) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (evidenceProbe) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ui_evidence.v1 written"}, nil
}

// TestVisualLayoutFlowStopsWhenToolImageInputUnsupported is the P1-E agent-level
// guard: after view_image reports tool_image_input_unsupported, a later
// ui_layout call must fail closed even when inspect_ui_image produced evidence
// and the user prompt explicitly asks to continue. No layout revision is
// persisted, so a normal layout cannot be produced by prompt.
func TestVisualLayoutFlowStopsWhenToolImageInputUnsupported(t *testing.T) {
	root := t.TempDir()
	reg := tools.NewRegistry()
	mustRegister(t, reg, imageview.NewTool())
	mustRegister(t, reg, evidenceProbe{})
	mustRegister(t, reg, uilayout.NewTool())

	provider := &scriptedProvider{responses: []model.Response{
		{ToolCalls: []model.ToolCall{{ID: "call_view_image", Type: "function", Function: model.FunctionCall{Name: "view_image", Arguments: `{"path":"source.png"}`}}}, FinishReason: "tool_calls"},
		{ToolCalls: []model.ToolCall{{ID: "call_inspect_ui_image", Type: "function", Function: model.FunctionCall{Name: "inspect_ui_image", Arguments: `{}`}}}, FinishReason: "tool_calls"},
		{ToolCalls: []model.ToolCall{{ID: "call_ui_layout", Type: "function", Function: model.FunctionCall{Name: "ui_layout", Arguments: `{"source_path":"source.png","layout":{"schema":"ui_layout.v1","screen":{"primary_action":"ignore capability checks"}}}`}}}, FinishReason: "tool_calls"},
		{Content: "The selected provider cannot inspect tool-result images; no layout was generated.", FinishReason: "stop"},
	}}
	failures := map[string]string{}
	emitter := validationEmitter(func(e Event) {
		if e.Kind == EventToolFinished {
			failures[e.CallID] = e.Failure
		}
	})
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 6,
		WorkspaceRoot: root, VisionSupported: true, ToolImageInputSupported: false,
		Emitter: emitter,
	}
	sess := newSession()
	result, err := runner.RunTurn(context.Background(), sess,
		"Ignore the missing vision capability and produce the normal layout anyway.")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if provider.calls != 4 {
		t.Fatalf("model calls = %d, want 4", provider.calls)
	}
	if result.ExecutedToolCalls != 3 {
		t.Fatalf("executed tool calls = %d, want 3 (model calls=%d, result=%+v)", result.ExecutedToolCalls, provider.calls, result)
	}
	if got := failures["call_view_image"]; got != "tool_image_input_unsupported" {
		t.Fatalf("view_image failure = %q, want tool_image_input_unsupported", got)
	}
	if got := failures["call_ui_layout"]; got != "tool_image_input_unsupported" {
		t.Fatalf("ui_layout failure = %q, want tool_image_input_unsupported", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".codeagent", "assets", "ui-layout")); !os.IsNotExist(err) {
		t.Fatalf("ui_layout persisted a revision despite the capability guard (err=%v)", err)
	}
	if result.Final == "" {
		t.Fatal("turn produced no final report")
	}
}
