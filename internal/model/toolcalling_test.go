package model

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNativeToolCallingRoundTrip verifies, end to end, that the configured
// model can:
//  1. receive a tool definition,
//  2. decide to call it,
//  3. accept the tool result,
//  4. produce a final text answer.
//
// It talks to a real OpenAI-compatible endpoint, so it is skipped unless
// DEEPSEEK_API_KEY is set. This is the Phase 1 gate: if the model never returns
// a tool call, it does not support function calling and must be swapped.
//
// Override the endpoint/model with CODEAGENT_TEST_BASE_URL / CODEAGENT_TEST_MODEL.
// Default model is deepseek-v4-flash (deepseek-chat / deepseek-reasoner are
// deprecated 2026-07-24 — do not rely on them).
func TestNativeToolCallingRoundTrip(t *testing.T) {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping live tool-calling test")
	}

	baseURL := getenvDefault("CODEAGENT_TEST_BASE_URL", "https://api.deepseek.com")
	modelName := getenvDefault("CODEAGENT_TEST_MODEL", "deepseek-v4-flash")

	provider := NewOpenAICompatibleProviderWithKey(baseURL, apiKey)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A real JSON Schema, not an example. In the next slice this will be
	// produced by tools.Registry instead of being hand-written here.
	listFilesSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Directory path relative to the workspace root."
			}
		},
		"required": ["path"]
	}`)

	tools := []ToolDefinition{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "list_files",
				Description: "List files and directories under a path in the workspace.",
				Parameters:  listFilesSchema,
			},
		},
	}

	messages := []Message{
		{Role: RoleSystem, Content: "You are a coding agent. Use the provided tools to inspect the workspace."},
		{Role: RoleUser, Content: "List the files in the current directory."},
	}

	// --- Turn 1: expect the model to request a tool call --------------------
	resp, err := provider.Complete(ctx, Request{
		Model:    modelName,
		Messages: messages,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}
	if !resp.HasToolCalls() {
		t.Fatalf("model did not request a tool call (finish=%q, content=%q). "+
			"This model may not support function calling; swap it.",
			resp.FinishReason, resp.Content)
	}

	call := resp.ToolCalls[0]
	if call.Function.Name != "list_files" {
		t.Fatalf("expected a list_files call, got %q", call.Function.Name)
	}
	t.Logf("turn 1: model requested tool=%s args=%s", call.Function.Name, call.Function.Arguments)

	// Arguments are a JSON-encoded string; validate before using.
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("could not parse tool arguments %q: %v", call.Function.Arguments, err)
	}
	if args.Path == "" {
		args.Path = "."
	}

	// --- Execute the tool locally (trivial stand-in for the real tool) ------
	entries, err := os.ReadDir(args.Path)
	if err != nil {
		t.Fatalf("list_files execution failed: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	toolResult := strings.Join(names, "\n")

	// --- Turn 2: feed the result back, expect a final text answer -----------
	messages = append(messages,
		resp.AssistantMessage(), // assistant turn carrying the tool_calls
		Message{
			Role:       RoleTool,
			ToolCallID: call.ID,
			Content:    toolResult,
		},
	)

	final, err := provider.Complete(ctx, Request{
		Model:    modelName,
		Messages: messages,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}
	if strings.TrimSpace(final.Content) == "" {
		t.Fatalf("expected a final text answer, got empty content (finish=%q)", final.FinishReason)
	}
	t.Logf("turn 2: final answer: %s", final.Content)
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
