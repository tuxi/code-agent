package agent

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
	todotool "code-agent/internal/tools/todo"
)

// TestStopPolicyExternalRejectsThenAccepts: a configured stop policy owns the
// finalize decision. It rejects the first finish with a nudge, accepts the
// second; the rejected finish is never persisted.
func TestStopPolicyExternalRejectsThenAccepts(t *testing.T) {
	reg := tools.NewRegistry()
	calls := 0
	pol := StopPolicyFunc(func(_ context.Context, _ StopContext) (StopVerdict, error) {
		calls++
		if calls == 1 {
			return StopVerdict{Continue: true, Message: "[policy] not yet — finish the design first"}, nil
		}
		return StopVerdict{}, nil
	})
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "first done", FinishReason: "stop"},
		{Content: "second done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Emitter: em, StopPolicy: pol}
	sess := newSession()

	res, err := runner.RunTurn(context.Background(), sess, "do the work")
	if err != nil {
		t.Fatal(err)
	}

	var nudge string
	for _, e := range em.events {
		if e.Kind == EventReflected && strings.HasPrefix(e.Text, "[policy]") {
			nudge = e.Text
		}
	}
	if nudge == "" {
		t.Fatal("expected the external policy's nudge to be emitted")
	}
	if !strings.Contains(nudge, "finish the design") {
		t.Errorf("nudge = %q", nudge)
	}
	if res.Final != "second done" {
		t.Errorf("final = %q, want %q", res.Final, "second done")
	}
	// The rejected first finish was never persisted.
	for _, m := range sess.Messages {
		if m.Role == model.RoleAssistant && m.Content == "first done" {
			t.Error("rejected finish 'first done' must not be persisted to history")
		}
	}
}

// TestStopPolicyExternalReplacesDefault: a configured policy REPLACES the
// built-in default entirely — even a pending checklist does not gate a finish
// the external policy accepts (replace semantics, not composition).
func TestStopPolicyExternalReplacesDefault(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(todotool.NewTool()); err != nil {
		t.Fatal(err)
	}
	pol := StopPolicyFunc(func(_ context.Context, _ StopContext) (StopVerdict, error) {
		return StopVerdict{}, nil // accept everything
	})
	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("todo_write", `{"todos":[{"content":"step one","status":"in_progress"}]}`),
		{Content: "done anyway", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Emitter: em, StopPolicy: pol}

	res, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range em.events {
		if e.Kind == EventReflected && strings.HasPrefix(e.Text, "[checklist]") {
			t.Error("the default todo gate must not run when an external policy replaces it")
		}
	}
	if res.Final != "done anyway" {
		t.Errorf("final = %q, want %q", res.Final, "done anyway")
	}
}

// TestStopPolicyErrorFailsClosed: a policy error must not silently accept the
// finish. It re-prompts with the reason; the step budget bounds the delay.
func TestStopPolicyErrorFailsClosed(t *testing.T) {
	calls := 0
	pol := StopPolicyFunc(func(context.Context, StopContext) (StopVerdict, error) {
		calls++
		if calls == 1 {
			return StopVerdict{}, context.DeadlineExceeded
		}
		return StopVerdict{}, nil
	})
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "done", FinishReason: "stop"},
		{Content: "done for real", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 5, Emitter: em, StopPolicy: pol}

	res, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, e := range em.events {
		if e.Kind == EventReflected && strings.HasPrefix(e.Text, "[policy]") {
			got = e.Text
		}
	}
	if !strings.Contains(got, "could not decide") {
		t.Errorf("expected a fail-closed policy message, got %q", got)
	}
	if res.Final != "done for real" {
		t.Errorf("final = %q, want %q", res.Final, "done for real")
	}
}
