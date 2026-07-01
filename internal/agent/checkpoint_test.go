package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

type countingCheckpointer struct{ n int }

func (c *countingCheckpointer) Checkpoint(_ context.Context, _ *session.Session) error {
	c.n++
	return nil
}

// okTool is a read-only no-op tool for driving multi-step turns without approval.
type okTool struct{}

func (okTool) Name() string                 { return "noop" }
func (okTool) Description() string          { return "read-only no-op" }
func (okTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (okTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ok"}, nil
}

// TestCheckpointerCalledPerToolIteration verifies the mid-turn checkpoint (v1.2 §2)
// fires at each consistent loop boundary — after a completed tool batch — and NOT
// on the final tool-free iteration, whose finish the caller's turn-boundary Save
// already persists.
func TestCheckpointerCalledPerToolIteration(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(okTool{}); err != nil {
		t.Fatalf("register noop: %v", err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		{ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "noop", Arguments: "{}"}}}, FinishReason: "tool_calls"},
		{ToolCalls: []model.ToolCall{{ID: "c2", Type: "function", Function: model.FunctionCall{Name: "noop", Arguments: "{}"}}}, FinishReason: "tool_calls"},
		{Content: "all done", FinishReason: "stop"},
	}}
	cp := &countingCheckpointer{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Checkpointer: cp}

	res, err := runner.RunTurn(context.Background(), newSession(), "do two things")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Final != "all done" {
		t.Errorf("final = %q, want %q", res.Final, "all done")
	}
	if cp.n != 2 {
		t.Errorf("checkpoint called %d times, want 2 (one per completed tool iteration)", cp.n)
	}
}

// TestNilCheckpointerIsSafe confirms an unset Checkpointer (the CLI/TUI path) is a
// no-op and does not affect the turn.
func TestNilCheckpointerIsSafe(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(okTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		{ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "noop", Arguments: "{}"}}}, FinishReason: "tool_calls"},
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5} // Checkpointer nil
	if _, err := runner.RunTurn(context.Background(), newSession(), "go"); err != nil {
		t.Fatalf("run with nil checkpointer failed: %v", err)
	}
}
