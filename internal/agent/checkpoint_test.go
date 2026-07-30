package agent

import (
	"context"
	"encoding/json"
	"testing"

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

func TestCheckpointerCalledPerToolIteration(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("noop", map[string]any{}),
		FauxTool("noop", map[string]any{}),
		FauxText("all done"),
	})
	if err := h.Tools.Register(okTool{}); err != nil {
		t.Fatalf("register noop: %v", err)
	}
	cp := &countingCheckpointer{}
	h.Runner.Checkpointer = cp

	res, err := h.RunTurn("do two things")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Final != "all done" {
		t.Errorf("final = %q, want %q", res.Final, "all done")
	}
	if cp.n != 2 {
		t.Errorf("checkpoint called %d times, want 2 (one per completed tool iteration)", cp.n)
	}
	// Verify the provider was called for both tool turns and the final answer.
	if h.Provider.CallCount != 3 {
		t.Errorf("CallCount = %d, want 3", h.Provider.CallCount)
	}
}

func TestNilCheckpointerIsSafe(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("noop", map[string]any{}),
		FauxText("done"),
	})
	if err := h.Tools.Register(okTool{}); err != nil {
		t.Fatal(err)
	}
	// Checkpointer is nil by default.
	if _, err := h.RunTurn("go"); err != nil {
		t.Fatalf("run with nil checkpointer failed: %v", err)
	}
}
