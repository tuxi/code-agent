package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/tools"
)

type assetTool struct{}

func (assetTool) Name() string                 { return "asset_tool" }
func (assetTool) Description() string          { return "returns a file asset" }
func (assetTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (assetTool) Execute(_ context.Context, ec tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	ref := assets.Ref{
		ID:                    "asset_from_tool",
		Kind:                  "file_location",
		WorkspaceRelativePath: "Sources/App.swift",
		Range:                 &assets.Range{StartLine: 5},
		DisplayName:           "App.swift:5",
		SourceTurnID:          ec.TurnID,
		SourceCallID:          ec.CallID,
	}
	return tools.ToolResult{
		Content: "Sources/App.swift:5: let value = 42",
		Assets:  []assets.Ref{ref},
	}, nil
}

func TestAnnotateTextWithAssets(t *testing.T) {
	text := "✅ See Sources/App.swift:5 and App.swift:5."
	refs := []assets.Ref{{
		ID:                    "asset_1",
		Kind:                  "file_location",
		WorkspaceRelativePath: "Sources/App.swift",
		Range:                 &assets.Range{StartLine: 5},
		DisplayName:           "App.swift:5",
		SourceTurnID:          "turn_1",
		SourceCallID:          "call_1",
	}}
	got := annotateTextWithAssets(text, refs)
	if len(got) != 2 {
		t.Fatalf("annotations = %d, want 2: %+v", len(got), got)
	}
	if got[0].Text != "Sources/App.swift:5" || got[0].AssetID != "asset_1" {
		t.Fatalf("first annotation = %+v", got[0])
	}
	if got[0].StartByte != len("✅ See ") {
		t.Fatalf("start_byte = %d", got[0].StartByte)
	}
	if got[0].StartUTF16 != 6 {
		t.Fatalf("start_utf16 = %d, want 6", got[0].StartUTF16)
	}
	if got[1].Text != "App.swift:5" {
		t.Fatalf("second annotation = %+v", got[1])
	}
}

func TestRunTurnEmitsAssistantTextAnnotations(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(assetTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		{
			ToolCalls: []model.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: model.FunctionCall{Name: "asset_tool", Arguments: "{}"},
			}},
			FinishReason: "tool_calls",
		},
		{Content: "Open `App.swift:5` for the important line.", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model:         provider,
		Tools:         reg,
		MaxSteps:      5,
		Emitter:       em,
		WorkspaceRoot: "/Users/x/project",
	}
	if _, err := runner.RunTurn(context.Background(), newSession(), "find asset"); err != nil {
		t.Fatal(err)
	}
	finished, ok := em.first(EventTurnFinished)
	if !ok {
		t.Fatal("turn_finished not emitted")
	}
	if len(finished.TextAnnotations) != 1 {
		t.Fatalf("annotations = %+v, want one", finished.TextAnnotations)
	}
	ann := finished.TextAnnotations[0]
	if ann.Text != "App.swift:5" || ann.AssetID != "asset_from_tool" || ann.SourceCallID != "call_1" {
		t.Fatalf("annotation = %+v", ann)
	}
}
