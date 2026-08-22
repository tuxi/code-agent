package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWireMessageTextOnly verifies the historical wire shape is preserved: a
// message without ContentParts serializes content as a plain string, so
// existing endpoints and golden tests see byte-identical behavior.
func TestWireMessageTextOnly(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "you are a coding agent"},
		{Role: RoleUser, Content: "read the file"},
		{Role: RoleAssistant, Content: "done", ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
		}}},
		{Role: RoleTool, Content: "contents", ToolCallID: "call_1"},
	}
	var got map[string]any
	data, err := json.Marshal(chatCompletionRequest{Messages: newWireMessages(msgs)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantContents := []string{"you are a coding agent", "read the file", "done", "contents"}
	messages := got["messages"].([]any)
	if len(messages) != len(wantContents) {
		t.Fatalf("messages = %#v, want %d", messages, len(wantContents))
	}
	for i, want := range wantContents {
		m := messages[i].(map[string]any)
		if got := m["content"]; got != want {
			t.Errorf("message %d content = %#v (%T), want string %q", i, got, got, want)
		}
	}
	// Tool-call pairing survives the wire conversion.
	toolMsg := messages[2].(map[string]any)
	if _, ok := toolMsg["tool_calls"]; !ok {
		t.Errorf("assistant message lost tool_calls: %#v", toolMsg)
	}
	toolResult := messages[3].(map[string]any)
	if toolResult["tool_call_id"] != "call_1" {
		t.Errorf("tool result tool_call_id = %#v, want call_1", toolResult["tool_call_id"])
	}
}

// TestWireMessageContentParts verifies the multimodal wire shape: a message
// carrying ContentParts serializes content as an ordered block array, with the
// text prompt leading and image_url data URLs following.
func TestWireMessageContentParts(t *testing.T) {
	msgs := []Message{{
		Role:    RoleUser,
		Content: "what is in this screenshot?",
		ContentParts: []ContentPart{
			{Type: "image_url", ImageURL: &ContentImage{URL: "data:image/png;base64,aGVsbG8="}},
		},
	}}
	var got map[string]any
	data, err := json.Marshal(chatCompletionRequest{Messages: newWireMessages(msgs)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	messages := got["messages"].([]any)
	blocks, ok := messages[0].(map[string]any)["content"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("content = %#v, want 2-block array", messages[0].(map[string]any)["content"])
	}
	text := blocks[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "what is in this screenshot?" {
		t.Errorf("text block = %#v, want leading text part", text)
	}
	img := blocks[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("image block = %#v, want image_url part", img)
	}
	url := img["image_url"].(map[string]any)["url"]
	if url != "data:image/png;base64,aGVsbG8=" {
		t.Errorf("image url = %#v, want data URL", url)
	}
}

// TestCompleteSendsContentPartsRoundTrip verifies the full provider path: parts
// assembled by the loop reach the request body as blocks, and the response is
// still parsed normally.
func TestCompleteSendsContentPartsRoundTrip(t *testing.T) {
	var request map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&request)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"a red circle"}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
	resp, err := p.Complete(context.Background(), Request{
		Model: "vision-test",
		Messages: []Message{{
			Role:    RoleUser,
			Content: "describe",
			ContentParts: []ContentPart{
				{Type: "image_url", ImageURL: &ContentImage{URL: "data:image/png;base64,cGly"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "a red circle" {
		t.Errorf("Content = %q, want a red circle", resp.Content)
	}
	messages := request["messages"].([]any)
	blocks, ok := messages[0].(map[string]any)["content"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("request content = %#v, want 2-block array", messages[0].(map[string]any)["content"])
	}
	if blocks[0].(map[string]any)["type"] != "text" || blocks[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("blocks = %#v, want text then image_url", blocks)
	}
}
