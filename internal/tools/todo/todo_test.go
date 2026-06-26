package todo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/tools"
)

func TestExecuteSummarizesList(t *testing.T) {
	res, err := NewTool().Execute(context.Background(), tools.ExecutionContext{},
		json.RawMessage(`{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"in_progress","activeForm":"doing b"}]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "2 total") || !strings.Contains(res.Content, "1 in progress") {
		t.Fatalf("summary = %q", res.Content)
	}
}

func TestAnnounceTodosParsesList(t *testing.T) {
	todos, ok := NewTool().AnnounceTodos(json.RawMessage(`{"todos":[{"content":"a","status":"completed"}]}`))
	if !ok || len(todos) != 1 || todos[0].Status != tools.TodoCompleted {
		t.Fatalf("AnnounceTodos = ok %v, %+v", ok, todos)
	}
}

func TestRejectsInvalidStatus(t *testing.T) {
	bad := json.RawMessage(`{"todos":[{"content":"a","status":"doing"}]}`)
	if _, err := NewTool().Execute(context.Background(), tools.ExecutionContext{}, bad); err == nil {
		t.Fatal("Execute should reject an unknown status")
	}
	if _, ok := NewTool().AnnounceTodos(bad); ok {
		t.Fatal("AnnounceTodos should be false on invalid input (no event emitted)")
	}
}

func TestRejectsEmptyContent(t *testing.T) {
	if _, err := NewTool().Execute(context.Background(), tools.ExecutionContext{},
		json.RawMessage(`{"todos":[{"content":"   ","status":"pending"}]}`)); err == nil {
		t.Fatal("Execute should reject empty content")
	}
}

func TestEmptyListClearsCleanly(t *testing.T) {
	todos, ok := NewTool().AnnounceTodos(json.RawMessage(`{"todos":[]}`))
	if !ok || len(todos) != 0 {
		t.Fatalf("an empty list is a valid clear: ok %v, %+v", ok, todos)
	}
}
