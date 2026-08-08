package agent

import (
	"code-agent/internal/hooks"
	"code-agent/internal/model"
	"testing"
)

func TestCtxHookFromModelRoundTrip(t *testing.T) {
	original := []model.Message{
		{Role: model.RoleSystem, Content: "You are a helpful assistant."},
		{Role: model.RoleUser, Content: "Hello"},
		{
			Role:    model.RoleAssistant,
			Content: "Let me help.",
			ToolCalls: []model.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: model.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"/tmp/test"}`,
					},
				},
			},
		},
		{
			Role:       model.RoleTool,
			Content:    "file contents here",
			ToolCallID: "call_1",
		},
	}

	// Convert to hooks representation and back.
	hookMsgs := ctxHookFromModel(original)
	restored := ctxHookToModel(hookMsgs)

	if len(restored) != len(original) {
		t.Fatalf("round-trip: got %d messages, want %d", len(restored), len(original))
	}

	for i := range original {
		if restored[i].Role != original[i].Role {
			t.Errorf("msg[%d].Role = %q, want %q", i, restored[i].Role, original[i].Role)
		}
		if restored[i].Content != original[i].Content {
			t.Errorf("msg[%d].Content = %q, want %q", i, restored[i].Content, original[i].Content)
		}
		if restored[i].ToolCallID != original[i].ToolCallID {
			t.Errorf("msg[%d].ToolCallID = %q, want %q", i, restored[i].ToolCallID, original[i].ToolCallID)
		}
		if len(restored[i].ToolCalls) != len(original[i].ToolCalls) {
			t.Errorf("msg[%d].ToolCalls count = %d, want %d", i, len(restored[i].ToolCalls), len(original[i].ToolCalls))
			continue
		}
		for j := range original[i].ToolCalls {
			if restored[i].ToolCalls[j].ID != original[i].ToolCalls[j].ID {
				t.Errorf("msg[%d].ToolCalls[%d].ID = %q, want %q", i, j, restored[i].ToolCalls[j].ID, original[i].ToolCalls[j].ID)
			}
			if restored[i].ToolCalls[j].Function.Name != original[i].ToolCalls[j].Function.Name {
				t.Errorf("msg[%d].ToolCalls[%d].Name = %q, want %q", i, j, restored[i].ToolCalls[j].Function.Name, original[i].ToolCalls[j].Function.Name)
			}
			if restored[i].ToolCalls[j].Function.Arguments != original[i].ToolCalls[j].Function.Arguments {
				t.Errorf("msg[%d].ToolCalls[%d].Args = %q, want %q", i, j, restored[i].ToolCalls[j].Function.Arguments, original[i].ToolCalls[j].Function.Arguments)
			}
		}
	}
}

func TestCtxHookFromModelEmptySlice(t *testing.T) {
	result := ctxHookFromModel(nil)
	if result == nil {
		t.Fatal("ctxHookFromModel(nil) should return non-nil slice")
	}
	if len(result) != 0 {
		t.Fatalf("got %d elements, want 0", len(result))
	}
}

func TestCtxHookToModelEmptySlice(t *testing.T) {
	result := ctxHookToModel(nil)
	if result == nil {
		t.Fatal("ctxHookToModel(nil) should return non-nil slice")
	}
	if len(result) != 0 {
		t.Fatalf("got %d elements, want 0", len(result))
	}
}

func TestCtxHookFromModelPreservesToolCalls(t *testing.T) {
	msgs := []model.Message{
		{
			Role: model.RoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "tc1", Type: "function", Function: model.FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}},
				{ID: "tc2", Type: "function", Function: model.FunctionCall{Name: "read", Arguments: `{"path":"f"}`}},
			},
		},
	}
	result := ctxHookFromModel(msgs)
	if len(result) != 1 {
		t.Fatalf("got %d messages", len(result))
	}
	if len(result[0].ToolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(result[0].ToolCalls))
	}
	if result[0].ToolCalls[0].ID != "tc1" {
		t.Errorf("tc[0].ID = %q", result[0].ToolCalls[0].ID)
	}
	if result[0].ToolCalls[1].Function.Name != "read" {
		t.Errorf("tc[1].Name = %q", result[0].ToolCalls[1].Function.Name)
	}
}

// Verify the bridge types match what hooks.RunContextHooks expects.
func TestCtxHookBridgeIntegrationShape(t *testing.T) {
	// Create a minimal message list and verify the hooks package can consume it.
	msgs := []model.Message{
		{Role: model.RoleSystem, Content: "system"},
		{Role: model.RoleUser, Content: "question"},
	}
	hookMsgs := ctxHookFromModel(msgs)

	// Manual run of context hooks with a no-op hook.
	runner := hooks.New([]hooks.Hook{
		{Event: hooks.ContextPreRequest, Command: "exit 0"},
	}, t.TempDir())
	result := runner.RunContextHooks(t.Context(), hookMsgs, "s1", "t1")
	if len(result) != 2 {
		t.Fatalf("no-op context hook: got %d messages, want 2", len(result))
	}
	if result[0].Role != "system" || result[0].Content != "system" {
		t.Errorf("unexpected msg[0]: %+v", result[0])
	}

	// Round-trip back.
	restored := ctxHookToModel(result)
	if len(restored) != 2 {
		t.Fatalf("round-trip: got %d", len(restored))
	}
}
