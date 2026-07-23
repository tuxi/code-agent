package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"code-agent/internal/tools"
)

type nestedRecordingTool struct {
	name    string
	effects bool
	calls   int
}

type immediateClientWaiter struct {
	callID string
	result ToolCallResult
}

func (w *immediateClientWaiter) Wait(_ context.Context, callID string, _ time.Duration) (ToolCallResult, error) {
	w.callID = callID
	return w.result, nil
}
func (*immediateClientWaiter) Deliver(string, ToolCallResult) {}
func (*immediateClientWaiter) CancelAll()                     {}

func TestExecuteNestedToolDispatchesStrictClientTool(t *testing.T) {
	waiter := &immediateClientWaiter{result: ToolCallResult{Content: "captured", Output: json.RawMessage(`{"asset_id":"photo-1"}`)}}
	runner := &Runner{ClientWaiter: waiter}
	tool := tools.NewClientProxyTool("capture_photo", "capture on device", json.RawMessage(`{"type":"object"}`))

	result, err := runner.ExecuteNestedTool(context.Background(), "parent", "workflow:camera:attempt-1", tool, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if waiter.callID != "workflow:camera:attempt-1" || result.Content != "captured" {
		t.Fatalf("client dispatch call_id=%q result=%q", waiter.callID, result.Content)
	}
}

func (t *nestedRecordingTool) Name() string        { return t.name }
func (t *nestedRecordingTool) Description() string { return t.name }
func (t *nestedRecordingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *nestedRecordingTool) SideEffects() bool { return t.effects }
func (t *nestedRecordingTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	t.calls++
	return tools.ToolResult{Content: "executed"}, nil
}

func TestExecuteNestedToolPreservesApprovalBoundary(t *testing.T) {
	tool := &nestedRecordingTool{name: "write", effects: true}
	runner := &Runner{WorkspaceRoot: t.TempDir()}

	result, err := runner.ExecuteNestedTool(context.Background(), "parent", "node-call", tool, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls != 0 {
		t.Fatal("side-effecting nested tool bypassed approval")
	}
	if result.Content != "The tool call was not approved. No changes were made." {
		t.Fatalf("unexpected denial result: %q", result.Content)
	}

	runner.Approver = allowApprover{}
	result, err = runner.ExecuteNestedTool(context.Background(), "parent", "node-call-2", tool, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls != 1 || result.Content != "executed" {
		t.Fatalf("approved nested tool did not execute: calls=%d result=%q", tool.calls, result.Content)
	}
}
