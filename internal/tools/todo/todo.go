// Package todo provides the `todo_write` tool: a structured checklist the model
// maintains for a multi-step task, so the user can watch progress (8.4). It is a
// whole-list-rewrite tool (the model sends the full list each call), which keeps
// it simple and robust — there are no item ids for the model to track across
// calls. It is read-only (it changes only the displayed checklist, never the
// workspace), so it runs without confirmation.
package todo

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type Tool struct{}

func NewTool() *Tool { return &Tool{} }

func (t *Tool) Name() string { return "todo_write" }

func (t *Tool) Description() string {
	return "Maintain a structured checklist for a multi-step task so the user can see progress. " +
		"Call it with the FULL list each time — it replaces the previous list. Use it for tasks " +
		"with 3+ distinct steps; skip it for trivial one-step work. Keep exactly ONE item " +
		"in_progress at a time: mark an item in_progress BEFORE you start it and completed " +
		"immediately AFTER it finishes — do not batch completions. Each item has content " +
		"(imperative, e.g. 'Add tests'), status (pending|in_progress|completed), and activeForm " +
		"(present tense shown while it runs, e.g. 'Adding tests')."
}

func (t *Tool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"todos": {
			Type:        "array",
			Description: "The full todo list; replaces the previous list on every call.",
			Items: &tools.Property{
				Type: "object",
				Properties: map[string]tools.Property{
					"content":    {Type: "string", Description: "The task in imperative form, e.g. 'Add tests'."},
					"status":     {Type: "string", Enum: []string{"pending", "in_progress", "completed"}, Description: "pending | in_progress | completed"},
					"activeForm": {Type: "string", Description: "Present-tense label shown while in_progress, e.g. 'Adding tests'."},
				},
				Required: []string{"content", "status"},
			},
		},
	}, "todos").JSON()
}

type input struct {
	Todos []tools.Todo `json:"todos"`
}

// parse decodes and validates the full list. A whole-list rewrite, so an empty
// list is valid (it clears the checklist).
func parse(raw json.RawMessage) ([]tools.Todo, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid todo input: %w", err)
	}
	for i, td := range in.Todos {
		switch td.Status {
		case tools.TodoPending, tools.TodoInProgress, tools.TodoCompleted:
		default:
			return nil, fmt.Errorf("todo %d: invalid status %q (want pending|in_progress|completed)", i+1, td.Status)
		}
		if strings.TrimSpace(td.Content) == "" {
			return nil, fmt.Errorf("todo %d: content is required", i+1)
		}
	}
	return in.Todos, nil
}

func (t *Tool) Execute(_ context.Context, raw json.RawMessage) (tools.ToolResult, error) {
	todos, err := parse(raw)
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: summarize(todos)}, nil
}

// AnnounceTodos lets the loop emit EventTodoUpdated without knowing this tool by
// name (the SkillAnnouncer pattern). It re-parses the same input Execute saw.
func (t *Tool) AnnounceTodos(raw json.RawMessage) ([]tools.Todo, bool) {
	todos, err := parse(raw)
	return todos, err == nil
}

func summarize(todos []tools.Todo) string {
	var pending, inProgress, completed int
	for _, td := range todos {
		switch td.Status {
		case tools.TodoInProgress:
			inProgress++
		case tools.TodoCompleted:
			completed++
		default:
			pending++
		}
	}
	return fmt.Sprintf("Todo list updated: %d total (%d pending, %d in progress, %d completed).",
		len(todos), pending, inProgress, completed)
}
