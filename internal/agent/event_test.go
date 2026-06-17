package agent

import (
	"context"
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

	if _, err := runner.RunTurn(context.Background(), newSession(), "do it"); err != nil {
		t.Fatal(err)
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
