package task

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeAgent struct {
	gotPrompt string
	result    string
	err       error
	called    bool
}

func (f *fakeAgent) Run(_ context.Context, _ string, prompt string) (string, error) {
	f.called = true
	f.gotPrompt = prompt
	return f.result, f.err
}

func TestExecuteForwardsPromptAndReturnsConclusion(t *testing.T) {
	fa := &fakeAgent{result: "the bug is a nil deref at loop.go:42"}
	res, err := NewTool(fa).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"prompt":"why does X fail?"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fa.gotPrompt != "why does X fail?" {
		t.Fatalf("forwarded prompt = %q", fa.gotPrompt)
	}
	if res.Content != "the bug is a nil deref at loop.go:42" {
		t.Fatalf("conclusion = %q", res.Content)
	}
}

func TestExecuteSurfacesRunError(t *testing.T) {
	fa := &fakeAgent{err: errors.New("subagent boom")}
	_, err := NewTool(fa).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"prompt":"go"}`))
	if err == nil || !strings.Contains(err.Error(), "subagent boom") {
		t.Fatalf("err = %v, want the subagent error", err)
	}
}

func TestExecuteRejectsEmptyPrompt(t *testing.T) {
	fa := &fakeAgent{}
	_, err := NewTool(fa).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"prompt":"   "}`))
	if err == nil {
		t.Fatal("expected an error for an empty prompt")
	}
	if fa.called {
		t.Fatal("subagent must not run on an empty prompt")
	}
}

func TestExecuteRejectsInvalidJSON(t *testing.T) {
	fa := &fakeAgent{}
	_, err := NewTool(fa).Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{not json`))
	if err == nil {
		t.Fatal("expected an error for invalid input")
	}
	if fa.called {
		t.Fatal("subagent must not run on invalid input")
	}
}

func TestSchemaRequiresPrompt(t *testing.T) {
	schema := string(NewTool(&fakeAgent{}).InputSchema())
	if !strings.Contains(schema, `"prompt"`) || !strings.Contains(schema, `"required"`) {
		t.Fatalf("schema should require prompt: %s", schema)
	}
}

func TestNameIsTask(t *testing.T) {
	if NewTool(&fakeAgent{}).Name() != "task" {
		t.Fatal("name must be task")
	}
}
