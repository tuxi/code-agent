package agent

import (
	"time"

	"code-agent/internal/tools"
)

// EventKind identifies what happened in the loop. Events are the agent runtime's
// event stream: the loop EMITS them and a subscriber (the REPL console today, a
// live-progress UI or a persisted event bus tomorrow) renders them. The loop
// itself never writes to stdout — that decoupling is what lets the same run feed
// a plain terminal, a "Thinking… 5s" ticker, or a remote UI unchanged.
type EventKind string

const (
	EventTurnStarted   EventKind = "turn_started"
	EventModelStarted  EventKind = "model_started"  // about to call the model
	EventModelFinished EventKind = "model_finished" // model returned (carries latency)
	EventTokenDelta    EventKind = "token_delta"    // a streamed text delta (8.6); ephemeral, not persisted
	EventThinking      EventKind = "thinking"       // model produced reasoning text
	EventToolStarted   EventKind = "tool_started"
	EventToolFinished  EventKind = "tool_finished"
	EventObserved      EventKind = "observed"      // a tool result was classified (P4.1)
	EventAutoApproved  EventKind = "auto_approved" // auto mode granted a side-effecting call without a human prompt (audit; p9.1 §12.3)
	EventReflected     EventKind = "reflected"     // a finalize self-check fired (P4.3)
	EventSkillLoaded   EventKind = "skill_loaded"  // a skill body was loaded (P6)
	EventTodoUpdated   EventKind = "todo_updated"  // the model's task checklist changed (8.4)
	EventCompacted     EventKind = "compacted"
	EventTurnFinished  EventKind = "turn_finished"

	// Subagent delegation (8.3). A `task` tool call brackets a nested run on an
	// isolated session with these events, so a renderer can present the delegation
	// distinctly (and keep the subagent's own event stream — tagged with its own
	// SessionID — in a collapsed sub-stream) instead of flooding the parent
	// timeline. Text carries the delegated prompt (Started) and the returned
	// conclusion (Finished).
	EventTaskStarted  EventKind = "task_started"
	EventTaskFinished EventKind = "task_finished"
)

// Event is a single point in a turn — a discriminated union where Kind selects
// which fields are meaningful. It is plain data (no behavior, no pointers into
// runtime state) so it can be rendered, logged, or sent over a wire unchanged.
type Event struct {
	Kind EventKind
	At   time.Time

	// Correlation IDs: which session and turn produced this event. A single
	// console reads them as constant, but a multiplexed bus (concurrent runs, a
	// web UI, DreamAI) needs them to keep streams from crossing.
	SessionID string
	TurnID    string

	// Tool events (ToolStarted / ToolFinished / Observed). For EventObserved,
	// Observation carries the one-line summary and Failure the FailureType.
	Step        int
	ToolName    string
	ToolArgs    string
	Observation string
	Failure     string // EventObserved: the classified FailureType (e.g. "compile")
	Version     string // EventSkillLoaded: the loaded skill's version (name is in ToolName)

	// Todos carries the model's current task checklist on EventTodoUpdated (8.4).
	Todos []tools.Todo

	// Model / thinking.
	Text         string        // reasoning text (Thinking) or final answer (TurnFinished)
	PromptTokens int           // ModelFinished
	Elapsed      time.Duration // ModelFinished: how long the model call took (P3.8 uses this)

	// Compaction (Compacted). AfterTokens == 0 means "just compacted, size not yet
	// measured"; > 0 means the next model call measured the reclaimed size.
	BeforeTokens int
	AfterTokens  int
	SavedTokens  int
	SummaryChars int
	Ratio        float64

	// Set when a step errored.
	Err string
}

// Emitter receives loop events. Implementations render (REPL), stream (live UI),
// or persist them. The Runner's emit helper is nil-safe, so an emitter is
// optional.
type Emitter interface {
	Emit(Event)
}
