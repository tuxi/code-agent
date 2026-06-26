package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/observation"
	"code-agent/internal/tools"
)

// jsonResultTool is a read-only tool that returns a fixed string — used to feed
// the loop a run_command-style result without running a real command.
type jsonResultTool struct {
	name string
	out  string
}

func (t *jsonResultTool) Name() string                 { return t.name }
func (t *jsonResultTool) Description() string          { return "returns a fixed result" }
func (t *jsonResultTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t *jsonResultTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: t.out}, nil
}

func runToolOnce(t *testing.T, toolName, toolOut string, observer Observer) *capturingEmitter {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(&jsonResultTool{name: toolName, out: toolOut}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		{
			ToolCalls: []model.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: model.FunctionCall{Name: toolName, Arguments: "{}"},
			}},
			FinishReason: "tool_calls",
		},
		{Content: "done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Emitter: em, Observer: observer}
	if _, err := runner.RunTurn(context.Background(), newSession(), "go"); err != nil {
		t.Fatal(err)
	}
	return em
}

// TestObserverEnrichesToolResult: with an Observer, a run_command failure result
// is classified (EventObserved) and the model-facing observation is enriched
// with the prepended [observation] block.
func TestObserverEnrichesToolResult(t *testing.T) {
	out := `{"command":"go build ./...","stderr":"# code-agent/internal/foo\ninternal/foo/service.go:42:13: undefined: Bar","exit_code":1,"decision":"allow"}`
	em := runToolOnce(t, "run_command", out, observation.DefaultObserver{})

	obs, ok := em.first(EventObserved)
	if !ok {
		t.Fatal("expected an EventObserved, none emitted")
	}
	if obs.Failure != "compile" {
		t.Errorf("observed failure = %q, want compile", obs.Failure)
	}
	if !strings.Contains(obs.Observation, "build failed") {
		t.Errorf("observed summary = %q, want it to mention build failed", obs.Observation)
	}

	tf, _ := em.first(EventToolFinished)
	if !strings.HasPrefix(tf.Observation, "[observation] failure=compile") {
		t.Errorf("tool result was not enriched with the observation block:\n%s", tf.Observation)
	}
	if !strings.Contains(tf.Observation, "undefined: Bar") {
		t.Errorf("enriched result lost the salient diagnostic:\n%s", tf.Observation)
	}
}

// TestNilObserverLeavesResultUnchanged: the loop stays neutral when no Observer
// is configured — no EventObserved, and the raw result is appended verbatim.
func TestNilObserverLeavesResultUnchanged(t *testing.T) {
	out := `{"command":"go build ./...","exit_code":1,"decision":"allow"}`
	em := runToolOnce(t, "run_command", out, nil)

	if _, ok := em.first(EventObserved); ok {
		t.Error("nil Observer must not emit EventObserved")
	}
	tf, _ := em.first(EventToolFinished)
	if strings.Contains(tf.Observation, "[observation]") {
		t.Errorf("nil Observer must not enrich the result, got:\n%s", tf.Observation)
	}
	if tf.Observation != out {
		t.Errorf("raw result changed: got %q, want %q", tf.Observation, out)
	}
}
