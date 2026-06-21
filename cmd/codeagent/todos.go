package main

import (
	"strings"

	"code-agent/internal/tools"
)

// renderTodos formats the model's checklist for the console: one line per item
// with a status mark; the in-progress item shows its present-tense activeForm.
func renderTodos(todos []tools.Todo) string {
	if len(todos) == 0 {
		return "[todos] (cleared)\n"
	}
	var b strings.Builder
	b.WriteString("[todos]\n")
	for _, td := range todos {
		b.WriteString("  " + todoMark(td.Status) + " " + todoLabel(td) + "\n")
	}
	return b.String()
}

func todoMark(s tools.TodoStatus) string {
	switch s {
	case tools.TodoCompleted:
		return "☑"
	case tools.TodoInProgress:
		return "▶"
	default:
		return "☐"
	}
}

func todoLabel(td tools.Todo) string {
	if td.Status == tools.TodoInProgress && td.ActiveForm != "" {
		return td.ActiveForm
	}
	return td.Content
}
