package agent

import (
	"fmt"
	"strings"

	"code-agent/internal/tools"
)

// hasPendingTodos reports whether the model's current checklist contains any
// item it has not completed (pending or in_progress). An empty (cleared) list
// and an all-completed list both converge — there is nothing to reconcile.
func hasPendingTodos(todos []tools.Todo) bool {
	for _, td := range todos {
		if td.Status != tools.TodoCompleted {
			return true
		}
	}
	return false
}

// todoReconcileNudge builds the one-shot ephemeral message the finalize gate
// injects when the model wants to finish with declared work outstanding. It
// lists exactly what is not completed and lets the model choose the reconcile:
// mark done what was finished, clear aspirational items, or state explicitly
// why it is stopping with these pending. The harness asks; the model owns
// control flow — forcing the work itself is out of scope for this gate.
func todoReconcileNudge(todos []tools.Todo) string {
	var pending []string
	for _, td := range todos {
		if td.Status == tools.TodoCompleted {
			continue
		}
		label := td.Content
		if td.Status == tools.TodoInProgress && td.ActiveForm != "" {
			label = td.ActiveForm
		}
		pending = append(pending, label)
	}
	var b strings.Builder
	b.WriteString("[checklist] Before finishing, reconcile your task checklist: it still shows ")
	fmt.Fprintf(&b, "%d item(s) not completed", len(pending))
	b.WriteString(":\n")
	for _, c := range pending {
		b.WriteString("  - ")
		b.WriteString(c)
		b.WriteByte('\n')
	}
	b.WriteString("Mark completed what you actually finished, clear aspirational items, or state explicitly " +
		"why you are stopping with these pending. Do not finish with a checklist that misrepresents your work.")
	return b.String()
}
