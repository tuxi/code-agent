package agent

import (
	"context"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// TestResumeTurnContinuesWithoutAppendingUser is the v1.2 §3.2 invariant: resuming
// re-enters the loop over the EXISTING history (which ends at a balanced tool
// batch) and drives it to a final answer, without injecting a new user message.
func TestResumeTurnContinuesWithoutAppendingUser(t *testing.T) {
	reg := tools.NewRegistry()
	// The paused history ends with a completed tool result, so the next model call
	// just produces the final answer.
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "resumed answer", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5}

	sess := newSession()
	// Simulate a checkpoint left mid-turn: user → assistant(tool_calls) → tool result.
	sess.Messages = append(sess.Messages,
		model.Message{Role: model.RoleUser, Content: "do the thing"},
		model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "noop", Arguments: "{}"}}}},
		model.Message{Role: model.RoleTool, ToolCallID: "c1", Content: "tool output"},
	)
	before := len(sess.Messages)

	res, err := runner.ResumeTurn(context.Background(), sess)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Final != "resumed answer" {
		t.Errorf("final=%q want %q", res.Final, "resumed answer")
	}
	// Exactly one message added (the assistant's final answer); no new user message.
	if got := len(sess.Messages) - before; got != 1 {
		t.Fatalf("added %d messages, want 1 (the final answer only)", got)
	}
	for _, m := range provider.lastMessages {
		if m.Role == model.RoleUser && m.Content != "do the thing" {
			t.Errorf("resume injected an unexpected user message: %q", m.Content)
		}
	}
}

// TestResumeTurnEmitsResumedEvent confirms the lifecycle event fires so a client
// can flip its label to "恢复中".
func TestResumeTurnEmitsResumedEvent(t *testing.T) {
	reg := tools.NewRegistry()
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok", FinishReason: "stop"}}}
	rec := &recordingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Emitter: rec}

	sess := newSession()
	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleUser, Content: "x"})

	if _, err := runner.ResumeTurn(context.Background(), sess); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !rec.saw(EventTurnResumed) {
		t.Error("ResumeTurn did not emit EventTurnResumed")
	}
}

type recordingEmitter struct{ kinds []EventKind }

func (r *recordingEmitter) Emit(e Event) { r.kinds = append(r.kinds, e.Kind) }
func (r *recordingEmitter) saw(k EventKind) bool {
	for _, got := range r.kinds {
		if got == k {
			return true
		}
	}
	return false
}
