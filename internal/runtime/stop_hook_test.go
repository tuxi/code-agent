package runtime

import (
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/hooks"
	"code-agent/internal/tools"
)

// stopHookInput maps the loop's StopContext into the hook JSON shape without
// losing fields — the mechanical glue between the StopPolicy bridge and the
// after_turn hook contract. Tested directly to avoid standing up a full
// BuildRunner (the user asked for the smallest harness, not the largest).
func TestStopHookInputMapping(t *testing.T) {
	in := stopHookInput(agent.StopContext{
		LastText:    "done",
		PlanState:   agent.PlanStatusExecuting,
		CodeMutated: true,
		ToolCalls:   7,
		MaxSteps:    24,
		Todos: []tools.Todo{
			{Content: "a", Status: tools.TodoCompleted},
			{Content: "b", Status: tools.TodoInProgress},
		},
	})
	want := hooks.StopHookInput{
		LastText:    "done",
		PlanState:   "executing",
		CodeMutated: true,
		ToolCalls:   7,
		MaxSteps:    24,
		Todos: []hooks.StopHookTodo{
			{Content: "a", Status: "completed"},
			{Content: "b", Status: "in_progress"},
		},
	}
	if in.LastText != want.LastText || in.PlanState != want.PlanState ||
		in.CodeMutated != want.CodeMutated || in.ToolCalls != want.ToolCalls ||
		in.MaxSteps != want.MaxSteps {
		t.Fatalf("scalar mapping mismatch:\n got %+v\nwant %+v", in, want)
	}
	if len(in.Todos) != len(want.Todos) {
		t.Fatalf("todos len = %d, want %d", len(in.Todos), len(want.Todos))
	}
	for i := range want.Todos {
		if in.Todos[i] != want.Todos[i] {
			t.Errorf("todos[%d] = %+v, want %+v", i, in.Todos[i], want.Todos[i])
		}
	}
}
