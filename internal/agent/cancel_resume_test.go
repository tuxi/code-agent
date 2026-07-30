package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// ── Cancel / Suspend tool doubles ──────────────────────────────────

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

// ctxAbortTool models a ctx-aware tool interrupted by Suspend: it cancels the
// turn context and returns context.Canceled instead of completing.
type ctxAbortTool struct{ cancel context.CancelFunc }

func (t *ctxAbortTool) Name() string                 { return "probe" }
func (t *ctxAbortTool) Description() string          { return "ctx-aware tool that aborts on cancel" }
func (t *ctxAbortTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t *ctxAbortTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	t.cancel()
	return tools.ToolResult{}, context.Canceled
}

// cancelProvider always fails with context.Canceled, simulating a model call
// aborted by an iOS Suspend mid-stream.
type cancelProvider struct{}

func (cancelProvider) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{}, context.Canceled
}

// ── Tests ──────────────────────────────────────────────────────────

func TestCancelMidBatchLeavesResumableHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ct := &cancelingTool{cancel: cancel}

	h := NewFauxHarness(t, []FauxStep{
		FauxTools(
			FauxToolCall{Name: "probe", Args: map[string]any{}},
			FauxToolCall{Name: "probe", Args: map[string]any{}},
			FauxToolCall{Name: "probe", Args: map[string]any{}},
		),
	})
	h.Tools.Register(ct)

	_, err := h.Runner.RunTurn(ctx, h.Session, "probe three times")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
	}
	if ct.ran != 1 {
		t.Fatalf("probe ran %d times, want exactly 1 (the rest cancelled before executing)", ct.ran)
	}
	assertBalancedToolCalls(t, h.Session.Messages)

	got := map[string]string{}
	for _, m := range h.Session.Messages {
		if m.Role == model.RoleTool {
			got[m.ToolCallID] = m.Content
		}
	}
	if got["faux_tc_0_0"] != "probed" {
		t.Errorf("call_1 result = %q, want %q", got["faux_tc_0_0"], "probed")
	}
	for _, id := range []string{"faux_tc_0_1", "faux_tc_0_2"} {
		if got[id] != toolInterruptedObservation {
			t.Errorf("%s result = %q, want interrupted marker", id, got[id])
		}
	}
}

func TestCancelBeforeAnyToolRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the turn starts

	h := NewFauxHarness(t, []FauxStep{
		FauxTools(
			FauxToolCall{Name: "probe", Args: map[string]any{}},
			FauxToolCall{Name: "probe", Args: map[string]any{}},
		),
	})
	if err := h.Tools.Register(&cancelingTool{cancel: cancel}); err != nil {
		t.Fatalf("register probe: %v", err)
	}

	_, err := h.Runner.RunTurn(ctx, h.Session, "probe")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
	}
	assertBalancedToolCalls(t, h.Session.Messages)
}

func TestSuspendDuringToolLeavesBalancedResumableHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	h := NewFauxHarness(t, []FauxStep{
		FauxTools(
			FauxToolCall{Name: "probe", Args: map[string]any{}},
			FauxToolCall{Name: "probe", Args: map[string]any{}},
		),
	})
	if err := h.Tools.Register(&ctxAbortTool{cancel: cancel}); err != nil {
		t.Fatalf("register probe: %v", err)
	}
	rec := &capturingEmitter{}
	h.Runner.Emitter = rec

	_, err := h.Runner.RunTurn(ctx, h.Session, "go")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
	}

	assertBalancedToolCalls(t, h.Session.Messages)

	toolResults := 0
	for _, m := range h.Session.Messages {
		if m.Role == model.RoleTool {
			toolResults++
			if m.Content != toolInterruptedObservation {
				t.Errorf("tool %s result = %q, want interrupted marker (no 'Tool error')", m.ToolCallID, m.Content)
			}
		}
	}
	if toolResults != 2 {
		t.Fatalf("got %d tool results, want 2 (balanced with the 2 calls)", toolResults)
	}
	for _, e := range rec.events {
		if e.Err != "" {
			t.Errorf("event %s surfaced an error on suspend: %q", e.Kind, e.Err)
		}
	}
}

func TestCancellationNotSurfacedAsError(t *testing.T) {
	rec := &capturingEmitter{}
	h := NewFauxHarness(t, []FauxStep{})
	h.Runner.Model = cancelProvider{}
	h.Runner.Emitter = rec

	_, err := h.Runner.RunTurn(context.Background(), h.Session, "hi")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn err = %v, want context.Canceled", err)
	}

	var sawModelFinished bool
	for _, e := range rec.events {
		if e.Err != "" {
			t.Errorf("event %s surfaced cancellation as an error: %q", e.Kind, e.Err)
		}
		if e.Kind == EventModelFinished {
			sawModelFinished = true
		}
	}
	if !sawModelFinished {
		t.Error("model_finished was not emitted on cancellation (ticker would leak)")
	}
}

// assertBalancedToolCalls enforces the resume invariant: every assistant message
// carrying tool calls is followed by exactly one tool result per call id.
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
