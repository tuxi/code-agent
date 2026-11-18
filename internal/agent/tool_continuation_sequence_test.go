package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	assets "code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// sequenceTool is a scripted tool with an arbitrary name: image tools return an
// image asset, the rest return plain text. It exists to reproduce the
// view_image → inspect_ui_image → ui_layout continuation without depending on
// real OCR or layout implementations.
type sequenceTool struct {
	name  string
	image string // absolute image path; empty for a text-only result
}

func (t sequenceTool) Name() string        { return t.name }
func (t sequenceTool) Description() string { return "test tool " + t.name }
func (t sequenceTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t sequenceTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	res := tools.ToolResult{Content: t.name + " result"}
	if t.image != "" {
		res.Content = "Viewed image"
		res.Assets = []assets.Ref{{ID: t.name + "-img", Kind: "image", MIMEType: "image/png", AbsolutePath: t.image}}
		res.VisionInputs = res.Assets
	}
	return res, nil
}

func toolCallSequence(names ...string) model.Response {
	calls := make([]model.ToolCall, len(names))
	for i, name := range names {
		calls[i] = model.ToolCall{
			ID:       "call_" + name,
			Type:     "function",
			Function: model.FunctionCall{Name: name, Arguments: `{}`},
		}
	}
	return model.Response{ToolCalls: calls, FinishReason: "tool_calls"}
}

// TestClientToolContinuationPersistsAdjacentMessages drives the exact P1-E
// sequence — view_image (image result) → inspect_ui_image → ui_layout — through
// the loop, then reloads the persisted session and asserts every assistant
// tool_calls message is directly followed by its own tool results. The synthetic
// image carrier the chat wire needs must never be persisted into that gap.
func TestClientToolContinuationPersistsAdjacentMessages(t *testing.T) {
	root := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	imgPath := filepath.Join(root, "shot.png")
	if err := os.WriteFile(imgPath, png, 0o600); err != nil {
		t.Fatal(err)
	}

	store, sess := runtimeFixture(t)
	reg := tools.NewRegistry()
	for _, tool := range []tools.Tool{
		sequenceTool{name: "view_image", image: imgPath},
		sequenceTool{name: "inspect_ui_image"},
		sequenceTool{name: "ui_layout"},
	} {
		mustRegister(t, reg, tool)
	}
	provider := &scriptedProvider{responses: []model.Response{
		toolCallSequence("view_image"),
		toolCallSequence("inspect_ui_image"),
		toolCallSequence("ui_layout"),
		{Content: "layout written", FinishReason: "stop"},
	}}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 8,
		WorkspaceRoot: root, VisionSupported: true, ToolImageInputSupported: true,
	}
	result, err := runner.RunTurn(context.Background(), sess, "adapt this UI")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if result.ExecutedToolCalls != 3 || result.Final != "layout written" {
		t.Fatalf("turn = %+v, want 3 executed calls and final text", result)
	}
	if provider.calls != 4 {
		t.Fatalf("model calls = %d, want 4", provider.calls)
	}

	// The request that follows the whole sequence is the one DeepSeek rejected:
	// assert its message order is still a valid tool conversation.
	assertToolAdjacency(t, provider.lastMessages, []string{"view_image", "inspect_ui_image", "ui_layout"})
	// The vision transformer must attach pixels to the tool result message itself;
	// the provider layer decides wire placement.
	if parts := toolMessage(provider.lastMessages, "call_view_image").ContentParts; len(parts) != 1 {
		t.Fatalf("view_image tool ContentParts = %+v, want one image part", parts)
	}

	// Persisted history: same invariant, and no synthetic user carrier in the gap.
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertToolAdjacency(t, loaded.Messages, []string{"view_image", "inspect_ui_image", "ui_layout"})
	for i, msg := range loaded.Messages {
		if msg.Role == model.RoleUser && i != 1 {
			t.Fatalf("persisted user message at %d: %+v; only the original user turn may be persisted", i, msg)
		}
	}
	assertBalancedToolCalls(t, loaded.Messages)
}

func toolMessage(messages []model.Message, callID string) model.Message {
	for _, msg := range messages {
		if msg.Role == model.RoleTool && msg.ToolCallID == callID {
			return msg
		}
	}
	return model.Message{}
}

// assertToolAdjacency checks the persisted/history ordering: every assistant
// tool_calls message is immediately followed by one tool message per call id,
// in call order, and the named tools appear in the expected step order.
func assertToolAdjacency(t *testing.T, messages []model.Message, wantTools []string) {
	t.Helper()
	var got []string
	for i, msg := range messages {
		if msg.Role != model.RoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}
		for j, call := range msg.ToolCalls {
			got = append(got, call.Function.Name)
			idx := i + 1 + j
			if idx >= len(messages) || messages[idx].Role != model.RoleTool || messages[idx].ToolCallID != call.ID {
				t.Fatalf("call %q at message %d is not immediately answered by its tool result; messages=%+v", call.ID, i, messages)
			}
		}
	}
	if len(got) != len(wantTools) {
		t.Fatalf("tool calls = %v, want %v", got, wantTools)
	}
	for i := range wantTools {
		if got[i] != wantTools[i] {
			t.Fatalf("tool calls = %v, want %v", got, wantTools)
		}
	}
}
