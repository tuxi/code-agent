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
func (s *sequencedTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
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

// P4.3-R Move 3: when the model is about to edit code AFTER a failure surfaced
// this turn, a one-shot pre-mutation self-check fires: the premature edit is
// dropped (not executed, not persisted) and re-prompted with a hypothesis nudge.
func TestPreMutationCheckFiresBeforeEditAfterFailure(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &sequencedTool{name: "run_command", outs: []string{goTestFailJSON, goTestOKJSON}})
	mustReg(t, reg, &jsonResultTool{name: "edit_file", out: "edited"})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("run_command", `{"command":"go test ./..."}`),     // fails
		toolCallResp("edit_file", `{"path":"internal/app/config.go"}`), // dropped by Move 3
		toolCallResp("edit_file", `{"path":"internal/app/config.go"}`), // the re-prompted edit
		toolCallResp("run_command", `{"command":"go test ./..."}`),     // passes
		{Content: "fixed the cause", FinishReason: "stop"},
	}}

	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer:         observation.DefaultObserver{},
		Reflector:        DefaultReflector{},
		RemindHypothesis: true,
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "make tests pass")
	if err != nil {
		t.Fatal(err)
	}

	// Exactly one pre-mutation check.
	n := 0
	for _, e := range em.events {
		if e.Kind == EventPreMutation {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("EventPreMutation count = %d, want 1 (one-shot)", n)
	}
	ev, _ := em.first(EventPreMutation)
	if !strings.Contains(ev.Text, "root-cause hypothesis") {
		t.Errorf("pre-mutation nudge missing the hypothesis prompt: %q", ev.Text)
	}
	if res.Final != "fixed the cause" {
		t.Errorf("final = %q, want %q", res.Final, "fixed the cause")
	}
}

// The check must NOT fire when no failure has surfaced: a first-pass edit on a
// clean turn proceeds untouched (precision — this is not a tax on every edit).
func TestPreMutationCheckSkippedWithoutFailure(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &jsonResultTool{name: "edit_file", out: "edited"})
	mustReg(t, reg, &sequencedTool{name: "run_command", outs: []string{goTestOKJSON}})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("edit_file", `{"path":"internal/app/feature.go"}`),
		toolCallResp("run_command", `{"command":"go build ./..."}`),
		{Content: "added the feature", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer:         observation.DefaultObserver{},
		Reflector:        DefaultReflector{},
		RemindHypothesis: true,
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "add a feature")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := em.first(EventPreMutation); ok {
		t.Error("no failure surfaced — the pre-mutation check must not fire")
	}
	if res.Final != "added the feature" {
		t.Errorf("final = %q, want %q", res.Final, "added the feature")
	}
}

// Opt-in: with RemindHypothesis off, an edit after a failure proceeds as before.
func TestPreMutationCheckOptIn(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &sequencedTool{name: "run_command", outs: []string{goTestFailJSON, goTestOKJSON}})
	mustReg(t, reg, &jsonResultTool{name: "edit_file", out: "edited"})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("run_command", `{"command":"go test ./..."}`),
		toolCallResp("edit_file", `{"path":"internal/app/config.go"}`),
		toolCallResp("run_command", `{"command":"go test ./..."}`),
		{Content: "done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer:  observation.DefaultObserver{},
		Reflector: DefaultReflector{}, // RemindHypothesis deliberately off
	}
	if _, err := runner.RunTurn(context.Background(), newSession(), "make tests pass"); err != nil {
		t.Fatal(err)
	}
	if _, ok := em.first(EventPreMutation); ok {
		t.Error("RemindHypothesis is off — the pre-mutation check must not fire")
	}
}

// P4.3-R Move 2 (2a): a turn that edits code without verifying it, with a
// VerifyCommand configured, runs the real verify at finalize. A PASS accepts the
// finish with no re-prompt — the change is genuinely confirmed, not guessed.
func TestFinalizeVerifyPassAcceptsFinish(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &jsonResultTool{name: "edit_file", out: "edited"})
	mustReg(t, reg, &sequencedTool{name: "run_command", outs: []string{goTestOKJSON}})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("edit_file", `{"path":"internal/app/config.go"}`),
		{Content: "done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer:      observation.DefaultObserver{},
		Reflector:     DefaultReflector{},
		VerifyCommand: "go test ./...",
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "change config")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := em.first(EventVerified); !ok {
		t.Error("expected a deterministic finalize verify to run")
	}
	if _, ok := em.first(EventReflected); ok {
		t.Error("a passing verify must not re-prompt")
	}
	if res.Final != "done" {
		t.Errorf("final = %q, want %q (passing verify accepts the finish)", res.Final, "done")
	}
}

// A FAILING verify feeds the real failure back and re-prompts; the model fixes
// and finishes. The verify runs at most once (the fix's finish is not re-verified).
func TestFinalizeVerifyFailReprompts(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &jsonResultTool{name: "edit_file", out: "edited"})
	mustReg(t, reg, &sequencedTool{name: "run_command", outs: []string{goTestFailJSON}})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("edit_file", `{"path":"internal/app/config.go"}`),
		{Content: "done", FinishReason: "stop"}, // premature — verify fails, re-prompted
		toolCallResp("edit_file", `{"path":"internal/app/config.go"}`),
		{Content: "fixed for real", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer:      observation.DefaultObserver{},
		Reflector:     DefaultReflector{},
		VerifyCommand: "go test ./...",
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "change config")
	if err != nil {
		t.Fatal(err)
	}
	nv := 0
	for _, e := range em.events {
		if e.Kind == EventVerified {
			nv++
		}
	}
	if nv != 1 {
		t.Fatalf("EventVerified count = %d, want 1 (verify runs once)", nv)
	}
	ev, ok := em.first(EventReflected)
	if !ok || !strings.Contains(ev.Text, "FAILED") {
		t.Errorf("expected a failure re-prompt carrying the real result, got %q", ev.Text)
	}
	if res.Final != "fixed for real" {
		t.Errorf("final = %q, want %q", res.Final, "fixed for real")
	}
}

// 2b: with NO VerifyCommand, an unverified code change is SILENT — the runtime
// never guesses "unverified". The first finish is accepted, no verify, no nudge.
func TestFinalizeNoVerifyCommandIsSilent(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, &jsonResultTool{name: "edit_file", out: "edited"})

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("edit_file", `{"path":"internal/app/config.go"}`),
		{Content: "done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 10, Emitter: em,
		Observer:  observation.DefaultObserver{},
		Reflector: DefaultReflector{}, // VerifyCommand deliberately empty
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "change config")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := em.first(EventVerified); ok {
		t.Error("no VerifyCommand — the runtime must not run a verify")
	}
	if _, ok := em.first(EventReflected); ok {
		t.Error("no VerifyCommand — the runtime must not nudge about 'unverified'")
	}
	if res.Final != "done" {
		t.Errorf("final = %q, want %q (first finish accepted silently)", res.Final, "done")
	}
}

func mustReg(t *testing.T, reg *tools.Registry, tool tools.Tool) {
	t.Helper()
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
}
