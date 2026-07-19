package agent

import (
	"context"
	"errors"
	"testing"

	"code-agent/internal/model"
)

// streamProvider implements model.StreamingProvider, emitting one delta per rune.
type streamProvider struct {
	text      string
	reasoning string
}

func (s streamProvider) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{Content: s.text, ReasoningContent: s.reasoning, FinishReason: "stop"}, nil
}
func (s streamProvider) CompleteStream(_ context.Context, _ model.Request, onText, onReasoning func(string)) (model.Response, error) {
	for _, r := range s.reasoning {
		onReasoning(string(r))
	}
	for _, r := range s.text {
		onText(string(r))
	}
	return model.Response{Content: s.text, ReasoningContent: s.reasoning, FinishReason: "stop"}, nil
}

func TestStreamSeparatesReasoningFromFinalText(t *testing.T) {
	em := &capturingEmitter{}
	runner := &Runner{
		Model:    streamProvider{text: "final answer", reasoning: "inspect auth flow"},
		Stream:   true,
		MaxSteps: 3,
		Emitter:  em,
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "final answer" {
		t.Fatalf("final=%q, want final answer", res.Final)
	}
	var textDeltas, reasoningDeltas string
	var snapshots []string
	for _, e := range em.events {
		switch e.Kind {
		case EventTokenDelta:
			textDeltas += e.Text
		case EventReasoningDelta:
			reasoningDeltas += e.Text
		case EventThinking:
			snapshots = append(snapshots, e.Text)
		}
	}
	if textDeltas != "final answer" || reasoningDeltas != "inspect auth flow" {
		t.Fatalf("text deltas=%q reasoning deltas=%q", textDeltas, reasoningDeltas)
	}
	if len(snapshots) != 1 || snapshots[0] != "inspect auth flow" {
		t.Fatalf("thinking snapshots=%q, want only provider reasoning", snapshots)
	}
}

func TestNonStreamResponseStillPublishesReasoningSnapshot(t *testing.T) {
	em := &capturingEmitter{}
	runner := &Runner{
		Model:    streamProvider{text: "done", reasoning: "checked constraints"},
		Stream:   false,
		MaxSteps: 3,
		Emitter:  em,
	}
	if _, err := runner.RunTurn(context.Background(), newSession(), "go"); err != nil {
		t.Fatal(err)
	}
	thinking, ok := em.first(EventThinking)
	if !ok || thinking.Text != "checked constraints" {
		t.Fatalf("thinking=%+v present=%v", thinking, ok)
	}
	for _, e := range em.events {
		if e.Kind == EventReasoningDelta {
			t.Fatal("non-stream completion must not emit reasoning_delta")
		}
	}
}

func TestStreamEmitsTokenDeltas(t *testing.T) {
	em := &capturingEmitter{}
	runner := &Runner{Model: streamProvider{text: "hi there"}, Stream: true, MaxSteps: 3, Emitter: em}
	res, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "hi there" {
		t.Fatalf("final = %q (streaming must not change the result)", res.Final)
	}
	var deltas string
	for _, e := range em.events {
		if e.Kind == EventTokenDelta {
			deltas += e.Text
		}
	}
	if deltas != "hi there" {
		t.Fatalf("token deltas = %q, want the full text", deltas)
	}
}

// failingStreamProvider streams some thinking text, then fails mid-stream and
// returns an empty Response — mirroring a provider that hits a read timeout
// after the body has partially arrived (and after its own accumulation is
// discarded).
type failingStreamProvider struct{ text string }

func (s failingStreamProvider) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{}, errors.New("model call failed: context deadline exceeded")
}
func (s failingStreamProvider) CompleteStream(_ context.Context, _ model.Request, _ func(string), onReasoning func(string)) (model.Response, error) {
	for _, r := range s.text {
		onReasoning(string(r))
	}
	return model.Response{}, errors.New("model call failed: context deadline exceeded")
}

// A failed streaming call must still persist the thinking that was shown live,
// so a later /events replay isn't missing it. Backfilled from the streamed
// deltas because the failed Response carries no content.
func TestThinkingPersistedWhenStreamFails(t *testing.T) {
	em := &capturingEmitter{}
	runner := &Runner{Model: failingStreamProvider{text: "reasoning step"}, Stream: true, MaxSteps: 3, Emitter: em}
	_, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err == nil {
		t.Fatal("expected the failed model call to return an error")
	}
	ev, ok := em.first(EventThinking)
	if !ok {
		t.Fatal("a failed streaming turn must still emit EventThinking with the live text")
	}
	if ev.Text != "reasoning step" {
		t.Fatalf("thinking text = %q, want the full streamed text", ev.Text)
	}
	// Ordering contract: thinking is emitted before the paired model_finished.
	kinds := em.kinds()
	thinkAt, finishAt := -1, -1
	for i, k := range kinds {
		if k == EventThinking && thinkAt == -1 {
			thinkAt = i
		}
		if k == EventModelFinished && finishAt == -1 {
			finishAt = i
		}
	}
	if thinkAt == -1 || finishAt == -1 || thinkAt > finishAt {
		t.Fatalf("expected thinking before model_finished, got kinds %v", kinds)
	}
	// model_finished only pairs the invocation. The executor will publish this
	// same error once as turn_failed, so putting it here would duplicate the
	// user-visible failure in persisted event history.
	for _, event := range em.events {
		if event.Kind == EventModelFinished && event.Err != "" {
			t.Fatalf("model_finished Err = %q, want empty terminal-pairing event", event.Err)
		}
	}
}

func TestNoTokenDeltasWhenStreamDisabled(t *testing.T) {
	em := &capturingEmitter{}
	runner := &Runner{Model: streamProvider{text: "hi"}, Stream: false, MaxSteps: 3, Emitter: em}
	if _, err := runner.RunTurn(context.Background(), newSession(), "go"); err != nil {
		t.Fatal(err)
	}
	for _, e := range em.events {
		if e.Kind == EventTokenDelta {
			t.Fatal("no token deltas should be emitted when Stream is off")
		}
	}
}
