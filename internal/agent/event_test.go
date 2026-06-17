package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

type capturingEmitter struct{ events []Event }

func (c *capturingEmitter) Emit(e Event) { c.events = append(c.events, e) }

func (c *capturingEmitter) kinds() []EventKind {
	out := make([]EventKind, len(c.events))
	for i, e := range c.events {
		out[i] = e.Kind
	}
	return out
}

func (c *capturingEmitter) first(k EventKind) (Event, bool) {
	for _, e := range c.events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

// A turn that calls one tool then answers must emit a coherent stream: a turn
// start, model start/finish around each call, the tool start/finish, and a turn
// finish carrying the final answer.
func TestRunTurnEmitsEventStream(t *testing.T) {
	rt := &recordingTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		{
			ToolCalls: []model.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: model.FunctionCall{Name: "danger", Arguments: "{}"},
			}},
			FinishReason: "tool_calls",
		},
		{Content: "all done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, Approver: allowApprover{}, MaxSteps: 5, Emitter: em}

	sess := newSession()
	sess.ID = "sess-x"
	if _, err := runner.RunTurn(context.Background(), sess, "do it"); err != nil {
		t.Fatal(err)
	}

	// Every event carries the same correlation IDs (session + turn).
	sid, tid := em.events[0].SessionID, em.events[0].TurnID
	if sid != "sess-x" || tid == "" {
		t.Fatalf("correlation ids = %q/%q, want sess-x/<non-empty>", sid, tid)
	}
	for _, e := range em.events {
		if e.SessionID != sid || e.TurnID != tid {
			t.Fatalf("event %s has mismatched ids %q/%q", e.Kind, e.SessionID, e.TurnID)
		}
	}

	// Turn boundaries.
	if k := em.kinds(); k[0] != EventTurnStarted {
		t.Fatalf("first event = %s, want turn_started", k[0])
	}
	if em.events[len(em.events)-1].Kind != EventTurnFinished {
		t.Fatalf("last event = %s, want turn_finished", em.events[len(em.events)-1].Kind)
	}

	// The tool call surfaced with its name and result.
	ts, ok := em.first(EventToolStarted)
	if !ok || ts.ToolName != "danger" || ts.Step != 1 {
		t.Fatalf("tool_started missing/wrong: %+v", ts)
	}
	tf, ok := em.first(EventToolFinished)
	if !ok || tf.Observation != "did it" {
		t.Fatalf("tool_finished missing/wrong: %+v", tf)
	}

	// Two model calls (tool turn + final), each bracketed by started/finished.
	var started, finished int
	for _, e := range em.events {
		switch e.Kind {
		case EventModelStarted:
			started++
		case EventModelFinished:
			finished++
		}
	}
	if started != 2 || finished != 2 {
		t.Fatalf("model started/finished = %d/%d, want 2/2", started, finished)
	}

	// The final answer rides on turn_finished.
	last := em.events[len(em.events)-1]
	if last.Text != "all done" {
		t.Fatalf("turn_finished text = %q, want %q", last.Text, "all done")
	}
}

// A canceled context must stop the turn at the next step boundary — not
// dead-reckon through the remaining MaxSteps. (Ctrl-C in the REPL wraps the
// turn ctx with signal.NotifyContext, which fires context.Canceled.)
func TestRunTurnStopsOnCanceledContext(t *testing.T) {
	reg := tools.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the turn starts

	runner := &Runner{Model: &scriptedProvider{}, Tools: reg, MaxSteps: 5}
	_, err := runner.RunTurn(ctx, newSession(), "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// Hitting the step limit must NOT discard the work: the loop makes one final
// tool-free call so the model answers from what it gathered, instead of
// returning the canned "stopped" message (which forced the user to re-ask).
func TestRunTurnSynthesizesAnswerAtStepLimit(t *testing.T) {
	rt := &recordingTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	toolCall := model.Response{
		ToolCalls: []model.ToolCall{{
			ID:       "c1",
			Type:     "function",
			Function: model.FunctionCall{Name: "danger", Arguments: "{}"},
		}},
		FinishReason: "tool_calls",
	}
	provider := &scriptedProvider{responses: []model.Response{
		toolCall, // step 0 (uses a tool, never finishes)
		toolCall, // step 1 — now MaxSteps=2 is reached
		{Content: "here is my best answer from what I gathered", FinishReason: "stop"}, // forced final
	}}
	runner := &Runner{Model: provider, Tools: reg, Approver: allowApprover{}, MaxSteps: 2}

	res, err := runner.RunTurn(context.Background(), newSession(), "investigate")
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "here is my best answer from what I gathered" {
		t.Fatalf("step-limit final = %q, want the synthesized answer (not the canned stop message)", res.Final)
	}
	// The forced final call must advertise NO tools.
	if len(provider.lastTools) != 0 {
		t.Fatalf("final synthesis call advertised %d tools, want 0", len(provider.lastTools))
	}
}

// Once a turn has made enough tool calls (>= half the step budget), the request
// must carry a transient convergence nudge — steering a restraint-poor model to
// answer rather than over-investigate. The nudge must not be persisted.
func TestRunTurnNudgesTowardConvergence(t *testing.T) {
	rt := &recordingTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	toolCall := model.Response{
		ToolCalls: []model.ToolCall{{
			ID: "c", Type: "function",
			Function: model.FunctionCall{Name: "danger", Arguments: "{}"},
		}},
		FinishReason: "tool_calls",
	}
	// MaxSteps=12 -> threshold 6. Six tool-call turns, then the 7th call (steps==6)
	// carries the nudge and the model finally answers.
	provider := &scriptedProvider{responses: []model.Response{
		toolCall, toolCall, toolCall, toolCall, toolCall, toolCall,
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, Approver: allowApprover{}, MaxSteps: 12}

	sess := newSession()
	if _, err := runner.RunTurn(context.Background(), sess, "investigate"); err != nil {
		t.Fatal(err)
	}

	// The final (over-threshold) request carried the nudge as its last message...
	last := provider.lastMessages[len(provider.lastMessages)-1]
	if last.Role != model.RoleUser || !strings.Contains(last.Content, "tool calls") {
		t.Fatalf("over-threshold call should carry the convergence nudge, got %+v", last)
	}
	// ...but the nudge must not be persisted into the session history.
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, "[reminder]") {
			t.Fatal("convergence nudge leaked into persisted history")
		}
	}
}

// With no Emitter set, the loop must run exactly the same (emit is nil-safe).
func TestRunTurnNilEmitterIsSafe(t *testing.T) {
	reg := tools.NewRegistry()
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "hi", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 3} // no Emitter
	res, err := runner.RunTurn(context.Background(), newSession(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "hi" {
		t.Fatalf("final = %q, want hi", res.Final)
	}
}
