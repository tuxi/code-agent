package agent

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

func TestLooksLikeToolCallLeak(t *testing.T) {
	leaks := []string{
		`<｜｜DSML｜｜tool_calls><｜｜DSML｜｜invoke name="read_file">cmd/x.go`,
		"reasoning… DSML …more",
		"prefix <｜ suffix",
	}
	for _, s := range leaks {
		if !LooksLikeToolCallLeak(s) {
			t.Errorf("should detect a leak: %q", s)
		}
	}
	// The key guard: a legitimate answer that DISCUSSES tool_calls is NOT a leak.
	clean := []string{
		"The loop emits EventTurnFinished; tool_calls are parsed by the provider.",
		"resp.ToolCalls is non-empty when the model requests tools.",
		"Here is the plan: edit loop.go and run the tests.",
		"",
	}
	for _, s := range clean {
		if LooksLikeToolCallLeak(s) {
			t.Errorf("false positive on legitimate prose: %q", s)
		}
	}
}

// leakAtLimitProvider returns tool calls while tools are offered, but leaks DSML
// markup as text on the forced no-tools final call (finalAnswerAfterLimit).
type leakAtLimitProvider struct{}

func (leakAtLimitProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	if len(req.Tools) == 0 {
		return model.Response{Content: `<｜｜DSML｜｜tool_calls><｜｜DSML｜｜invoke name="x">`}, nil
	}
	return model.Response{ToolCalls: []model.ToolCall{{
		ID: "c1", Type: "function", Function: model.FunctionCall{Name: "danger", Arguments: "{}"},
	}}}, nil
}

func TestFinalAnswerAfterLimitSanitizesLeak(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(&recordingTool{}); err != nil { // "danger" — keeps the loop calling tools
		t.Fatal(err)
	}
	sess := newSession()
	runner := &Runner{Model: leakAtLimitProvider{}, Tools: reg, Approver: allowApprover{}, MaxSteps: 2}

	res, err := runner.RunTurn(context.Background(), sess, "go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Final, "DSML") {
		t.Fatalf("leaked markup must not reach the user as the final answer: %q", res.Final)
	}
	if res.Final != stepLimitMessage {
		t.Fatalf("a leaked final answer should fall back to the step-limit message, got %q", res.Final)
	}
	if !res.HitStepLimit {
		t.Fatal("the turn should have hit the step limit")
	}
	// The garbage must not be persisted to history either.
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, "DSML") {
			t.Fatal("leaked markup must not be written to the session history")
		}
	}
}
