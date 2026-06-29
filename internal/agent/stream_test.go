package agent

import (
	"context"
	"errors"
	"testing"

	"code-agent/internal/model"
)

// streamProvider implements model.StreamingProvider, emitting one delta per rune.
type streamProvider struct{ text string }

func (s streamProvider) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{Content: s.text, FinishReason: "stop"}, nil
}
func (s streamProvider) CompleteStream(_ context.Context, _ model.Request, onText func(string)) (model.Response, error) {
	for _, r := range s.text {
		onText(string(r))
	}
	return model.Response{Content: s.text, FinishReason: "stop"}, nil
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
func (s failingStreamProvider) CompleteStream(_ context.Context, _ model.Request, onText func(string)) (model.Response, error) {
	for _, r := range s.text {
		onText(string(r))
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
