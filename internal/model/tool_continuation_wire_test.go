package model

import (
	"encoding/json"
	"testing"
)

// continuationMessages models one turn with three consecutive client-visible
// tool steps: view_image (returns an image), inspect_ui_image, ui_layout. The
// first tool result carries an image part, which the chat-completions wire has
// to relocate; the later steps are what a naive relocation breaks.
func continuationMessages() []Message {
	return []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "adapt this UI"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{
			ID: "call_view", Type: "function",
			Function: FunctionCall{Name: "view_image", Arguments: `{"path":"shot.png"}`},
		}}},
		{Role: RoleTool, ToolCallID: "call_view", Content: "Viewed image shot.png.", ContentParts: []ContentPart{{
			Type:     "image_url",
			ImageURL: &ContentImage{URL: "data:image/png;base64,c2hvdA=="},
		}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{
			ID: "call_inspect", Type: "function",
			Function: FunctionCall{Name: "inspect_ui_image", Arguments: `{"path":"shot.png"}`},
		}}},
		{Role: RoleTool, ToolCallID: "call_inspect", Content: "ui_evidence.v1 written"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{
			ID: "call_layout", Type: "function",
			Function: FunctionCall{Name: "ui_layout", Arguments: `{}`},
		}}},
		{Role: RoleTool, ToolCallID: "call_layout", Content: "revision-000001"},
		{Role: RoleAssistant, Content: "done"},
	}
}

// wireRoleID is the (role, tool_call_id) pair used to check wire ordering.
type wireRoleID struct {
	role string
	id   string
}

func chatWireRoles(t *testing.T, messages []Message) []wireRoleID {
	t.Helper()
	var got struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	data, err := json.Marshal(chatCompletionRequest{Messages: newWireMessages(messages)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make([]wireRoleID, len(got.Messages))
	for i, m := range got.Messages {
		out[i] = wireRoleID{role: m.Role, id: m.ToolCallID}
	}
	return out
}

// assertToolCallsAdjacent enforces the provider invariant on a wire/index list:
// right after an assistant message carrying tool calls, every call id must be
// answered by consecutive tool messages before any other role appears.
func assertToolCallsAdjacent(t *testing.T, calls map[int][]string, roles []wireRoleID) {
	t.Helper()
	for idx, ids := range calls {
		if idx+1 >= len(roles) {
			t.Fatalf("assistant tool_calls at %d has no following message", idx)
		}
		answered := map[string]bool{}
		for _, id := range ids {
			answered[id] = false
		}
		for j := idx + 1; j < len(roles) && roles[j].role == "tool"; j++ {
			if _, ok := answered[roles[j].id]; ok {
				answered[roles[j].id] = true
			}
		}
		for id, ok := range answered {
			if !ok {
				t.Fatalf("assistant tool_calls at %d: call %q is not followed by its tool message; wire=%+v", idx, id, roles)
			}
		}
		if roles[idx+1].role != "tool" {
			t.Fatalf("assistant tool_calls at %d is followed by %q, want a tool message; wire=%+v", idx, roles[idx+1].role, roles)
		}
	}
}

// TestChatWireMultiStepContinuationKeepsToolAdjacency locks the P1-E regression:
// a tool-result image that is promoted to a synthetic user message must be
// flushed BEFORE the next assistant message, never between that assistant's
// tool_calls and its tool results (DeepSeek: "tool_calls must be followed by
// tool messages").
func TestChatWireMultiStepContinuationKeepsToolAdjacency(t *testing.T) {
	roles := chatWireRoles(t, continuationMessages())
	want := []wireRoleID{
		{role: "system"},
		{role: "user"},
		{role: "assistant"},
		{role: "tool", id: "call_view"},
		{role: "user"}, // synthetic image carrier, after the tool run
		{role: "assistant"},
		{role: "tool", id: "call_inspect"},
		{role: "assistant"},
		{role: "tool", id: "call_layout"},
		{role: "assistant"},
	}
	if len(roles) != len(want) {
		t.Fatalf("wire messages = %+v, want %d", roles, len(want))
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("wire[%d] = %+v, want %+v; full wire=%+v", i, roles[i], want[i], roles)
		}
	}
	assertToolCallsAdjacent(t, map[int][]string{
		2: {"call_view"},
		5: {"call_inspect"},
		7: {"call_layout"},
	}, roles)
}

// TestResponsesMultiStepContinuationKeepsCallOutputAdjacency is the Responses
// counterpart: each replayed function_call item is immediately followed by its
// function_call_output, in the original step order, and the tool image rides
// inside that output rather than as a free-standing item.
func TestResponsesMultiStepContinuationKeepsCallOutputAdjacency(t *testing.T) {
	body := toResponsesRequest(Request{Messages: continuationMessages()}, false)
	if len(body.Input) != 8 {
		t.Fatalf("input items = %d (%+v), want 8", len(body.Input), body.Input)
	}
	want := []struct {
		typ    string
		callID string
	}{
		{"message", ""},
		{"function_call", "call_view"},
		{"function_call_output", "call_view"},
		{"function_call", "call_inspect"},
		{"function_call_output", "call_inspect"},
		{"function_call", "call_layout"},
		{"function_call_output", "call_layout"},
		{"message", ""},
	}
	for i, w := range want {
		if body.Input[i].Type != w.typ || body.Input[i].CallID != w.callID {
			t.Fatalf("input[%d] = %+v, want type=%s call_id=%s", i, body.Input[i], w.typ, w.callID)
		}
	}
	for i := 1; i < len(body.Input)-1; i += 2 {
		if body.Input[i].Type != "function_call" {
			t.Fatalf("input[%d] = %+v, want function_call", i, body.Input[i])
		}
		if body.Input[i+1].Type != "function_call_output" || body.Input[i+1].CallID != body.Input[i].CallID {
			t.Fatalf("input[%d] does not immediately answer %q: %+v", i+1, body.Input[i].CallID, body.Input[i+1])
		}
	}
	blocks, ok := body.Input[2].Output.([]responseContent)
	if !ok || len(blocks) != 2 || blocks[1].Type != "input_image" {
		t.Fatalf("view_image output = %#v, want text + input_image blocks", body.Input[2].Output)
	}
}
