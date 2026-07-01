package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// cancelingTool is a read-only probe that cancels the turn context the first
// time it runs, simulating an iOS Suspend / Ctrl-C landing in the middle of a
// multi-tool batch.
type cancelingTool struct {
	cancel context.CancelFunc
	ran    int
}

func (t *cancelingTool) Name() string                 { return "probe" }
func (t *cancelingTool) Description() string          { return "read-only probe that cancels on first run" }
func (t *cancelingTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t *cancelingTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	t.ran++
	t.cancel()
	return tools.ToolResult{Content: "probed"}, nil
}

// TestCancelMidBatchLeavesResumableHistory is the v1.2 §2.2 invariant: when the
// context is cancelled partway through a multi-tool-call batch, RunTurn must
// balance the assistant tool_calls message with one result per call (real for
// the calls that ran, an interrupted marker for the rest) before returning, so
// the persisted history is valid to resend to the provider on resume.
func TestCancelMidBatchLeavesResumableHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ct := &cancelingTool{cancel: cancel}
	reg := tools.NewRegistry()
	if err := reg.Register(ct); err != nil {
		t.Fatalf("register probe: %v", err)
	}

	// One assistant message requesting three tool calls; the first cancels the
	// context, so the loop must fill the remaining two with balanced results.
	provider := &scriptedProvider{responses: []model.Response{{
		ToolCalls: []model.ToolCall{
			{ID: "call_1", Type: "function", Function: model.FunctionCall{Name: "probe", Arguments: "{}"}},
			{ID: "call_2", Type: "function", Function: model.FunctionCall{Name: "probe", Arguments: "{}"}},
			{ID: "call_3", Type: "function", Function: model.FunctionCall{Name: "probe", Arguments: "{}"}},
		},
		FinishReason: "tool_calls",
	}}}

	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5}
	sess := newSession()

	_, err := runner.RunTurn(ctx, sess, "probe three times")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
	}
	if ct.ran != 1 {
		t.Fatalf("probe ran %d times, want exactly 1 (the rest cancelled before executing)", ct.ran)
	}
	assertBalancedToolCalls(t, sess.Messages)

	// call_1 has a real result; the interrupted calls carry the marker.
	got := map[string]string{}
	for _, m := range sess.Messages {
		if m.Role == model.RoleTool {
			got[m.ToolCallID] = m.Content
		}
	}
	if got["call_1"] != "probed" {
		t.Errorf("call_1 result = %q, want %q", got["call_1"], "probed")
	}
	for _, id := range []string{"call_2", "call_3"} {
		if got[id] != toolInterruptedObservation {
			t.Errorf("%s result = %q, want interrupted marker", id, got[id])
		}
	}
}

// TestCancelBeforeAnyToolRuns covers the earliest cancellation window: the
// context is already cancelled when the batch begins, so no tool runs yet the
// assistant message still gets a full set of interrupted results.
func TestCancelBeforeAnyToolRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the turn starts

	reg := tools.NewRegistry()
	if err := reg.Register(&cancelingTool{cancel: cancel}); err != nil {
		t.Fatalf("register probe: %v", err)
	}
	provider := &scriptedProvider{responses: []model.Response{{
		ToolCalls: []model.ToolCall{
			{ID: "a", Type: "function", Function: model.FunctionCall{Name: "probe", Arguments: "{}"}},
			{ID: "b", Type: "function", Function: model.FunctionCall{Name: "probe", Arguments: "{}"}},
		},
		FinishReason: "tool_calls",
	}}}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5}
	sess := newSession()

	// A context cancelled before the first model call short-circuits at the outer
	// loop's checkpoint (no assistant message appended), which is already
	// resumable. If the model call slips through, the batch fill keeps it balanced.
	_, err := runner.RunTurn(ctx, sess, "probe")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
	}
	assertBalancedToolCalls(t, sess.Messages)
}

// assertBalancedToolCalls enforces the resume invariant: every assistant message
// carrying tool calls is followed by exactly one tool result per call id, so the
// history is valid to resend to the provider.
func assertBalancedToolCalls(t *testing.T, msgs []model.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role != model.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, tc := range m.ToolCalls {
			want[tc.ID] = true
		}
		for _, r := range msgs[i+1:] {
			if r.Role != model.RoleTool {
				break
			}
			delete(want, r.ToolCallID)
		}
		if len(want) != 0 {
			t.Fatalf("assistant message at index %d has tool calls without matching results: %v", i, want)
		}
	}
}
