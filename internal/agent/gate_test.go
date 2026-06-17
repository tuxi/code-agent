package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

// scriptedProvider returns queued responses in order and records the messages
// it last received, so tests can assert what context reached the model.
type scriptedProvider struct {
	responses    []model.Response
	calls        int
	lastMessages []model.Message
	lastTools    []model.ToolDefinition
}

func (p *scriptedProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.lastMessages = req.Messages
	p.lastTools = req.Tools
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

func newSession() *session.Session {
	return &session.Session{
		Messages: []model.Message{{Role: model.RoleSystem, Content: "You are a test agent."}},
		Metadata: map[string]any{},
	}
}

func runGated(t *testing.T, approver Approver) (*recordingTool, TurnResult) {
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
		{Content: "done", FinishReason: "stop"}, // final answer
	}}

	runner := &Runner{Model: provider, Tools: reg, Approver: approver, MaxSteps: 5}

	res, err := runner.RunTurn(context.Background(), newSession(), "do the dangerous thing")
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
	for _, s := range res.Steps {
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

// TestSessionContinuityAcrossTurns is the core P2.2 invariant: a second turn
// must see the first turn's messages, because the Session persists history.
func TestSessionContinuityAcrossTurns(t *testing.T) {
	reg := tools.NewRegistry()
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "noted", FinishReason: "stop"},
		{Content: "you said hello", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 3}
	sess := newSession()

	if _, err := runner.RunTurn(context.Background(), sess, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunTurn(context.Background(), sess, "what did I say?"); err != nil {
		t.Fatal(err)
	}

	var sawFirstUser bool
	for _, m := range provider.lastMessages {
		if m.Role == model.RoleUser && m.Content == "hello" {
			sawFirstUser = true
		}
	}
	if !sawFirstUser {
		t.Error("second turn did not see the first turn's message; session is not accumulating history")
	}

	// system + (user + assistant) + (user + assistant) = 5
	if len(sess.Messages) != 5 {
		t.Errorf("session holds %d messages, want 5", len(sess.Messages))
	}
}
