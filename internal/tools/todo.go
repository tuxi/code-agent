package tools

import "encoding/json"

// TodoStatus is the lifecycle state of one checklist item (8.4).
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

// Todo is one item in the model's task checklist. Content is the imperative task
// ("Add tests"); ActiveForm is the present-tense label shown while it runs
// ("Adding tests"). Plain data, so it rides on an Event and persists in the event
// store unchanged.
type Todo struct {
	Content    string     `json:"content"`
	ActiveForm string     `json:"active_form,omitempty"`
	Status     TodoStatus `json:"status"`
}

// TodoAnnouncer is implemented by the todo_write tool: after it runs, the loop
// asks it for the current list and emits an EventTodoUpdated — the same
// interface-driven, loop-stays-tool-agnostic pattern as SkillAnnouncer. ok is
// false when the input did not parse, so no event is emitted for a bad call.
type TodoAnnouncer interface {
	AnnounceTodos(input json.RawMessage) (todos []Todo, ok bool)
}
