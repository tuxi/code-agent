package agent

import "time"

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
	EventThinking      EventKind = "thinking"       // model produced reasoning text
	EventToolStarted   EventKind = "tool_started"
	EventToolFinished  EventKind = "tool_finished"
	EventObserved      EventKind = "observed"     // a tool result was classified (P4.1)
	EventReflected     EventKind = "reflected"    // a finalize self-check fired (P4.3)
	EventSkillLoaded   EventKind = "skill_loaded" // a skill body was loaded (P6)
	EventCompacted     EventKind = "compacted"
	EventTurnFinished  EventKind = "turn_finished"
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
