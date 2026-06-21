package agent

import (
	"context"
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
