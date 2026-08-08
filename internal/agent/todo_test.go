package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
	todotool "code-agent/internal/tools/todo"
)

// fakeTodoTool implements tools.TodoAnnouncer, so the loop should emit an
// EventTodoUpdated carrying the list after it runs. Its announced list is
// all-completed so the turn's finalize passes the todo gate (an in_progress
// item would re-prompt and exhaust the scripted provider below).
type fakeTodoTool struct{}

func (fakeTodoTool) Name() string                 { return "todo_write" }
func (fakeTodoTool) Description() string          { return "todos" }
func (fakeTodoTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (fakeTodoTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ok"}, nil
}
func (fakeTodoTool) AnnounceTodos(json.RawMessage) ([]tools.Todo, bool) {
	return []tools.Todo{{Content: "step one", Status: tools.TodoCompleted}}, true
}

func TestTodoUpdatedEvent(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(fakeTodoTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("todo_write", `{"todos":[{"content":"step one","status":"in_progress"}]}`),
		{Content: "done", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Emitter: em}

	if _, err := runner.RunTurn(context.Background(), newSession(), "go"); err != nil {
		t.Fatal(err)
	}

	ev, ok := em.first(EventTodoUpdated)
	if !ok {
		t.Fatal("no EventTodoUpdated emitted")
	}
	if len(ev.Todos) != 1 || ev.Todos[0].Content != "step one" || ev.Todos[0].Status != tools.TodoCompleted {
		t.Errorf("EventTodoUpdated todos = %+v", ev.Todos)
	}
}

// TestTodoGateReconcile: a turn that writes a checklist with an uncompleted
// item and then finalizes must trigger exactly one todo-gate nudge; the model
// reconciles by rewriting the list to all-completed; the first "done" is
// re-prompted, never persisted.
func TestTodoGateReconcile(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, todotool.NewTool())

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("todo_write", `{"todos":[{"content":"step one","status":"in_progress"},{"content":"step two","status":"pending"}]}`),
		{Content: "done", FinishReason: "stop"},
		toolCallResp("todo_write", `{"todos":[{"content":"step one","status":"completed"},{"content":"step two","status":"completed"}]}`),
		{Content: "done for real", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 10, Emitter: em}
	runner.StopPolicy = &TodoGate{}
	sess := newSession()

	res, err := runner.RunTurn(context.Background(), sess, "do the steps")
	if err != nil {
		t.Fatal(err)
	}

	// Exactly one todo-gate nudge, naming the pending item.
	var nudge string
	n := 0
	for _, e := range em.events {
		if e.Kind == EventReflected && strings.HasPrefix(e.Text, "[checklist]") {
			n++
			nudge = e.Text
		}
	}
	if n != 1 {
		t.Fatalf("todo gate nudges = %d, want 1", n)
	}
	if !strings.Contains(nudge, "step two") {
		t.Errorf("nudge should name the pending item, got %q", nudge)
	}

	if res.Final != "done for real" {
		t.Errorf("final = %q, want %q", res.Final, "done for real")
	}
	// The premature "done" was re-prompted, not persisted.
	for _, m := range sess.Messages {
		if m.Role == model.RoleAssistant && m.Content == "done" {
			t.Error("premature finalize 'done' must not be persisted to history")
		}
	}
}

// An all-completed checklist converges: no nudge, first finish accepted.
func TestTodoGateSkipsWhenConverged(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, todotool.NewTool())

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("todo_write", `{"todos":[{"content":"step one","status":"completed"}]}`),
		{Content: "all good", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Emitter: em}

	res, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range em.events {
		if e.Kind == EventReflected && strings.HasPrefix(e.Text, "[checklist]") {
			t.Error("converged checklist must not trigger the todo gate")
		}
	}
	if res.Final != "all good" {
		t.Errorf("final = %q, want %q", res.Final, "all good")
	}
}

// No checklist at all: the gate is a no-op.
func TestTodoGateSkipsWhenNoTodos(t *testing.T) {
	reg := tools.NewRegistry()
	provider := &scriptedProvider{responses: []model.Response{
		{Content: "all good", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 5, Emitter: em}

	res, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range em.events {
		if e.Kind == EventReflected && strings.HasPrefix(e.Text, "[checklist]") {
			t.Error("no checklist must not trigger the todo gate")
		}
	}
	if res.Final != "all good" {
		t.Errorf("final = %q, want %q", res.Final, "all good")
	}
}

// An explicit clear (empty list) is a valid reconcile: the gate fires once,
// then the cleared checklist converges and the finish is accepted.
func TestTodoGateSkipsWhenCleared(t *testing.T) {
	reg := tools.NewRegistry()
	mustReg(t, reg, todotool.NewTool())

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("todo_write", `{"todos":[{"content":"step one","status":"in_progress"}]}`),
		{Content: "done", FinishReason: "stop"},
		toolCallResp("todo_write", `{"todos":[]}`),
		{Content: "cleared", FinishReason: "stop"},
	}}
	em := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 10, Emitter: em}
	runner.StopPolicy = &TodoGate{}

	res, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range em.events {
		if e.Kind == EventReflected && strings.HasPrefix(e.Text, "[checklist]") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("todo gate nudges = %d, want 1 (cleared list converges after one nudge)", n)
	}
	if res.Final != "cleared" {
		t.Errorf("final = %q, want %q", res.Final, "cleared")
	}
}
