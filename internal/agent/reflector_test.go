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

// sequencedTool returns a different fixed output on each call (then repeats the
// last), so a fake "run_command" can fail first and pass later.
type sequencedTool struct {
	name string
	outs []string
	i    int
}

func (s *sequencedTool) Name() string                 { return s.name }
func (s *sequencedTool) Description() string          { return "sequenced fake tool" }
func (s *sequencedTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (s *sequencedTool) Execute(context.Context, json.RawMessage) (tools.ToolResult, error) {
	out := s.outs[len(s.outs)-1]
	if s.i < len(s.outs) {
		out = s.outs[s.i]
	}
	s.i++
	return tools.ToolResult{Content: out}, nil
}

func toolCallResp(name, args string) model.Response {
	return model.Response{
		ToolCalls:    []model.ToolCall{{Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}},
		FinishReason: "tool_calls",
	}
}

const (
	goTestFailJSON = `{"command":"go test ./...","stdout":"--- FAIL: TestX (0.00s)\n    x_test.go:1: bad\nFAIL\tpkg 0.1s","exit_code":1,"decision":"allow"}`
	goTestOKJSON   = `{"command":"go test ./...","stdout":"ok  pkg 0.1s","exit_code":0,"decision":"allow"}`
)

// A paper-over turn (test fails → edit the TEST → test passes → "done") must
// trigger exactly one reflection pass, continue, and accept the *second* answer.
// The premature "done" must never be persisted.
func TestReflectionTriggersOneSelfCheck(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &sequencedTool{name: "run_command", outs: []string{goTestFailJSON, goTestOKJSON}})
	mustReg(t, reg, &jsonResultTool{name: "edit_file", out: "edited"})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("run_command", `{"command":"go test ./..."}`),
		toolCallResp("edit_file", `{"path":"internal/app/config_test.go"}`),
		toolCallResp("run_command", `{"command":"go test ./..."}`),
		{Content: "done", FinishReason: "stop"},
		{Content: "verified for real", FinishReason: "stop"},
	}}

	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer:  observation.DefaultObserver{},
		Reflector: DefaultReflector{},
	}
	sess := newSession()
	res, err := runner.RunTurn(context.Background(), sess, "make tests pass")
	if err != nil {
		t.Fatal(err)
	}

	// Exactly one reflection pass.
	n := 0
	for _, e := range em.events {
		if e.Kind == EventReflected {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("EventReflected count = %d, want 1 (one-shot)", n)
	}

	// The nudge named the edited test file.
	ev, _ := em.first(EventReflected)
	if !strings.Contains(ev.Text, "config_test.go") {
		t.Errorf("reflection nudge missing the test file: %q", ev.Text)
	}

	// The accepted answer is the post-reflection one.
	if res.Final != "verified for real" {
		t.Errorf("final = %q, want %q", res.Final, "verified for real")
	}

	// The premature "done" was re-prompted, not persisted.
	for _, m := range sess.Messages {
		if m.Role == model.RoleAssistant && m.Content == "done" {
			t.Error("premature finalize 'done' must not be persisted to history")
		}
	}
}

// A clean turn (verify passes, no test edited) must not reflect.
func TestReflectionSkippedWhenClean(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &sequencedTool{name: "run_command", outs: []string{goTestOKJSON}})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("run_command", `{"command":"go test ./..."}`),
		{Content: "all good", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer:  observation.DefaultObserver{},
		Reflector: DefaultReflector{},
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "run the tests")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := em.first(EventReflected); ok {
		t.Error("a clean turn must not trigger reflection")
	}
	if res.Final != "all good" {
		t.Errorf("final = %q, want %q", res.Final, "all good")
	}
}

// With no Reflector, the first "done" is accepted as before (P4.3 is opt-in).
func TestNilReflectorAcceptsFirstFinish(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &sequencedTool{name: "run_command", outs: []string{goTestFailJSON, goTestOKJSON}})
	mustReg(t, reg, &jsonResultTool{name: "edit_file", out: "edited"})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("run_command", `{"command":"go test ./..."}`),
		toolCallResp("edit_file", `{"path":"internal/app/config_test.go"}`),
		toolCallResp("run_command", `{"command":"go test ./..."}`),
		{Content: "done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer: observation.DefaultObserver{}, // Reflector deliberately nil
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "make tests pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := em.first(EventReflected); ok {
		t.Error("nil Reflector must not reflect")
	}
	if res.Final != "done" {
		t.Errorf("final = %q, want %q (first finish accepted)", res.Final, "done")
	}
}

func mustReg(t *testing.T, reg *tools.Registry, tool tools.Tool) {
	t.Helper()
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
}
