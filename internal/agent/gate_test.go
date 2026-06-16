package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// scriptedProvider returns queued responses in order, ignoring the request.
type scriptedProvider struct {
	responses []model.Response
	calls     int
}

func (p *scriptedProvider) Complete(_ context.Context, _ model.Request) (model.Response, error) {
	r := p.responses[p.calls]
	p.calls++
	return r, nil
}

// recordingTool is a side-effecting tool that records whether it actually ran.
type recordingTool struct{ ran bool }

func (t *recordingTool) Name() string                 { return "danger" }
func (t *recordingTool) Description() string          { return "a side-effecting tool" }
func (t *recordingTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t *recordingTool) SideEffects() bool            { return true }
func (t *recordingTool) Execute(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
	t.ran = true
	return tools.ToolResult{Content: "did it"}, nil
}

type allowApprover struct{}

func (allowApprover) Approve(string, json.RawMessage) bool { return true }

type denyApprover struct{}

func (denyApprover) Approve(string, json.RawMessage) bool { return false }

func runGated(t *testing.T, approver Approver) (*recordingTool, RunResult) {
	t.Helper()

	rt := &recordingTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatalf("register danger: %v", err)
	}

	provider := &scriptedProvider{responses: []model.Response{
		{ // turn 1: call the side-effecting tool
			ToolCalls: []model.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: model.FunctionCall{Name: "danger", Arguments: "{}"},
			}},
			FinishReason: "tool_calls",
		},
		{Content: "done", FinishReason: "stop"}, // turn 2: final answer
	}}

	runner := &Runner{
		Model:    provider,
		Tools:    reg,
		Approver: approver,
		MaxSteps: 5,
	}

	res, err := runner.Run(context.Background(), "do the dangerous thing")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return rt, res
}

func TestGateDeniesSideEffectingTool(t *testing.T) {
	rt, res := runGated(t, denyApprover{})

	if rt.ran {
		t.Fatal("tool ran despite the approver denying it")
	}
	if res.Final != "done" {
		t.Errorf("final = %q, want %q", res.Final, "done")
	}

	var sawDecline bool
	for _, s := range res.State.Steps {
		if strings.Contains(s.Observation, "declined") {
			sawDecline = true
		}
	}
	if !sawDecline {
		t.Error("expected the model to be told the user declined")
	}
}

func TestGateAllowsSideEffectingTool(t *testing.T) {
	rt, _ := runGated(t, allowApprover{})
	if !rt.ran {
		t.Fatal("tool did not run despite the approver allowing it")
	}
}

func TestGateNilApproverDeniesByDefault(t *testing.T) {
	rt, res := runGated(t, nil)
	if rt.ran {
		t.Fatal("nil approver must fail safe and deny side-effecting tools")
	}
	if res.Final != "done" {
		t.Errorf("final = %q, want %q", res.Final, "done")
	}
}
