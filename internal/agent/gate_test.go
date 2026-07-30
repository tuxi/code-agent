package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

// ── Shared test helpers (used by multiple test files) ───────────────

// scriptedProvider returns queued responses in order and records the messages
// it last received, so tests can assert what context reached the model.
type scriptedProvider struct {
	responses    []model.Response
	calls        int
	lastMessages []model.Message
	lastTools    []model.ToolDefinition
	lastRequest  model.Request
}

func (p *scriptedProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.lastMessages = req.Messages
	p.lastTools = req.Tools
	p.lastRequest = req
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
func (t *recordingTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	t.ran = true
	return tools.ToolResult{Content: "did it"}, nil
}

type allowApprover struct{}

func (allowApprover) Approve(string, json.RawMessage) Verdict { return VerdictAllow }

type denyApprover struct{}

func (denyApprover) Approve(string, json.RawMessage) Verdict { return VerdictDeny }

func newSession() *session.Session {
	return &session.Session{
		Messages: []model.Message{{Role: model.RoleSystem, Content: "You are a test agent."}},
		Metadata: map[string]any{},
	}
}

// ── Tests ───────────────────────────────────────────────────────────

func TestEmptyAssistantResponseFailsWithoutPersistingNoOp(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{FauxText("   ")})

	_, err := h.RunTurn("hello")
	if !errors.Is(err, model.ErrEmptyAssistantResponse) {
		t.Fatalf("RunTurn error = %v, want ErrEmptyAssistantResponse", err)
	}
	if len(h.Session.Messages) != 2 {
		t.Fatalf("messages = %+v, want system + user only", h.Session.Messages)
	}
	if got := h.Session.Messages[1]; got.Role != model.RoleUser || got.Content != "hello" {
		t.Fatalf("last message = %+v, want original user turn", got)
	}
}

func TestLegacyEmptyAssistantNoOpIsRemovedBeforeNextRequest(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{FauxText("recovered")})
	h.Session.Messages = append(h.Session.Messages,
		model.Message{Role: model.RoleAssistant, Content: "", ToolCalls: nil},
		model.Message{Role: model.RoleUser, Content: "previous request"},
	)

	if _, err := h.RunTurn("next request"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	req := h.LastRequest()
	if len(req.Messages) == 0 || req.Messages[0].Role != model.RoleSystem {
		t.Fatalf("request messages = %+v", req.Messages)
	}
	for _, message := range req.Messages {
		if message.IsEmptyAssistantNoOp() {
			t.Fatalf("legacy no-op leaked into provider request: %+v", message)
		}
	}
}

func TestGateDeniesSideEffectingTool(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("danger", map[string]any{}),
		FauxText("done"),
	})
	tool := h.RegisterSideEffectingTool("danger", "a side-effecting tool", "did it")
	h.Runner.Approver = denyApprover{}

	res, err := h.RunTurn("do the dangerous thing")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if tool.Ran {
		t.Fatal("tool ran despite the approver denying it")
	}
	if res.Final != "done" {
		t.Errorf("final = %q, want %q", res.Final, "done")
	}
	var sawDecline bool
	for _, s := range res.Steps {
		if strings.Contains(s.Observation, "not approved") {
			sawDecline = true
		}
	}
	if !sawDecline {
		t.Error("expected the model to be told the tool was not approved")
	}
}

func TestGateAllowsSideEffectingTool(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("danger", map[string]any{}),
		FauxText("done"),
	})
	tool := h.RegisterSideEffectingTool("danger", "a side-effecting tool", "did it")
	h.Runner.Approver = allowApprover{}

	if _, err := h.RunTurn("do the dangerous thing"); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !tool.Ran {
		t.Fatal("tool did not run despite the approver allowing it")
	}
}

func TestGateNilApproverDeniesByDefault(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("danger", map[string]any{}),
		FauxText("done"),
	})
	tool := h.RegisterSideEffectingTool("danger", "a side-effecting tool", "did it")

	res, err := h.RunTurn("do the dangerous thing")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if tool.Ran {
		t.Fatal("nil approver must fail safe and deny side-effecting tools")
	}
	if res.Final != "done" {
		t.Errorf("final = %q, want %q", res.Final, "done")
	}
}

func TestSessionContinuityAcrossTurns(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{FauxText("noted")})

	if _, err := h.RunTurn("hello"); err != nil {
		t.Fatal(err)
	}

	h.Provider.AppendResponses([]FauxStep{FauxText("you said hello")})
	if _, err := h.RunTurn("what did I say?"); err != nil {
		t.Fatal(err)
	}

	req := h.LastRequest()
	var sawFirstUser bool
	for _, m := range req.Messages {
		if m.Role == model.RoleUser && m.Content == "hello" {
			sawFirstUser = true
		}
	}
	if !sawFirstUser {
		t.Error("second turn did not see the first turn's message; session is not accumulating history")
	}
	if len(h.Session.Messages) != 5 {
		t.Errorf("session holds %d messages, want 5", len(h.Session.Messages))
	}
}
