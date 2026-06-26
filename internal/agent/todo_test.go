package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// fakeTodoTool implements tools.TodoAnnouncer, so the loop should emit an
// EventTodoUpdated carrying the list after it runs.
type fakeTodoTool struct{}

func (fakeTodoTool) Name() string                 { return "todo_write" }
func (fakeTodoTool) Description() string          { return "todos" }
func (fakeTodoTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (fakeTodoTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ok"}, nil
}
func (fakeTodoTool) AnnounceTodos(json.RawMessage) ([]tools.Todo, bool) {
	return []tools.Todo{{Content: "step one", ActiveForm: "doing step one", Status: tools.TodoInProgress}}, true
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
	if len(ev.Todos) != 1 || ev.Todos[0].Content != "step one" || ev.Todos[0].Status != tools.TodoInProgress {
		t.Errorf("EventTodoUpdated todos = %+v", ev.Todos)
	}
}
