package agent

import (
	"encoding/json"
	"time"

	"code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// EventKind identifies what happened in the loop. Events are the agent runtime's
// event stream: the loop EMITS them and a subscriber (the REPL console today, a
// live-progress UI or a persisted event bus tomorrow) renders them. The loop
// itself never writes to stdout — that decoupling is what lets the same run feed
// a plain terminal, a "Thinking… 5s" ticker, or a remote UI unchanged.
type EventKind string

const (
	EventTurnAccepted   EventKind = "turn_accepted"
	EventTurnQueued     EventKind = "turn_queued"
	EventTurnStarted    EventKind = "turn_started"
	EventModelStarted   EventKind = "model_started"   // about to call the model
	EventModelFinished  EventKind = "model_finished"  // model returned (carries latency)
	EventTokenDelta     EventKind = "token_delta"     // streamed final-answer text; ephemeral, not persisted
	EventReasoningDelta EventKind = "reasoning_delta" // streamed provider-visible reasoning; ephemeral, not persisted
	EventThinking       EventKind = "thinking"        // complete provider-visible reasoning snapshot; persisted
	EventToolStarted    EventKind = "tool_started"
	EventToolStdout     EventKind = "tool_stdout" // a stdout chunk during tool execution
	EventToolStderr     EventKind = "tool_stderr" // a stderr chunk during tool execution
	EventToolFinished   EventKind = "tool_finished"
	EventObserved       EventKind = "observed"      // a tool result was classified (P4.1)
	EventAutoApproved   EventKind = "auto_approved" // auto mode granted a side-effecting call without a human prompt (audit; p9.1 §12.3)
	EventReflected      EventKind = "reflected"     // a finalize self-check fired (P4.3)
	EventPreMutation    EventKind = "pre_mutation"  // a pre-mutation root-cause self-check fired (P4.3-R Move 3)
	EventVerified       EventKind = "verified"      // a deterministic finalize verify ran (P4.3-R Move 2)
	EventSkillLoaded    EventKind = "skill_loaded"  // a skill body was loaded (P6)
	EventTodoUpdated    EventKind = "todo_updated"  // the model's task checklist changed (8.4)
	EventCompacted      EventKind = "compacted"
	// EventContextPruned: tier-0 deterministic pruning ran (P12.c) — old tool
	// results truncated / think-blocks stripped outside the protected tail, with
	// no LLM call. SavedTokens carries the approximate reclaimed size; the true
	// size is measured by the next model call, like compaction.
	EventContextPruned EventKind = "context_pruned"
	EventTurnFinished  EventKind = "turn_finished"

	// Lifecycle (v1.2). Emitted around suspend/resume so a client can drive the
	// "已暂停 / 恢复中 / 思考中" UI labels off the event stream. TurnResumed fires
	// when ResumeTurn re-enters the loop; TurnPaused/TurnFailed are emitted by the
	// lifecycle layer (executor/embed) at the suspend and unrecoverable-resume
	// boundaries.
	EventTurnResumed   EventKind = "turn_resumed"
	EventTurnPaused    EventKind = "turn_paused"
	EventTurnFailed    EventKind = "turn_failed"
	EventTurnCancelled EventKind = "turn_cancelled"

	// Subagent delegation (8.3). A `task` tool call brackets a nested run on an
	// isolated session with these events, so a renderer can present the delegation
	// distinctly (and keep the subagent's own event stream — tagged with its own
	// SessionID — in a collapsed sub-stream) instead of flooding the parent
	// timeline. Text carries the delegated prompt (Started) and the returned
	// conclusion (Finished).
	EventTaskStarted  EventKind = "task_started"
	EventTaskFinished EventKind = "task_finished"

	// Plan mode (10.1). PlanProposed fires when the model calls propose_plan.
	// Text carries the full plan content. PlanApproved/PlanRejected fire after the
	// user's verdict; Text carries the plan ID.
	EventPlanProposed EventKind = "plan_proposed"
	EventPlanApproved EventKind = "plan_approved"
	EventPlanRejected EventKind = "plan_rejected"

	// Human-in-the-Loop task clarification. AskUserPosted fires when the model
	// calls ask_user with a question and options. AskUserResolved fires when the
	// user answers. AskUserTimeout fires when no user is available (headless,
	// auto mode) and the tool returns its fallback message.
	EventAskUserPosted  EventKind = "ask_user_posted"
	EventAskUserResolved EventKind = "ask_user_resolved"
	EventAskUserTimeout  EventKind = "ask_user_timeout"

	// Background job observability (P8.7 Phase A). A background run_command's
	// lifecycle as events, persisted under the JOB's own id partition
	// (SessionID = job id — the same partitioning the subagent uses for its
	// sub-session transcript), so GET /v1/conversations/{job_id}/events replays a
	// job's full life without a single job_status poll. JobStarted's Text carries
	// the command; JobOutput's Chunk carries a coalesced output span; JobFinished's
	// Text carries the terminal status (exited/failed/canceled), Elapsed the
	// duration, ExitCode the process exit code (0 stays zero-valued: absent on
	// the wire — status alone distinguishes success), and Err an exit-code note
	// when it failed.
	EventJobStarted  EventKind = "job_started"
	EventJobOutput   EventKind = "job_output"
	EventJobFinished EventKind = "job_finished"

	EventSessionRepaired EventKind = "session_repaired"
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
	SessionID     string
	TurnID        string
	RequestID     string // client-generated idempotency/correlation key for agent_input
	UserAssets    []model.GatewayAssetRef
	QueuePosition int    // EventTurnQueued: one-based scheduler position
	QueueReason   string // EventTurnQueued: global_capacity | workspace_lease | session_serialization

	// Seq is the monotonic per-store sequence number assigned when the event is
	// persisted (v1.2 §4). It is 0 on the core path and stamped by the transport
	// layer — live by the sequencing emitter, on read by the replay endpoint — so a
	// reconnecting client can ask for only events with a greater seq. omitempty so
	// it never bloats the persisted payload (the seq lives in the row, not the blob).
	Seq int64 `json:"seq,omitempty"`

	// InvocationID groups events produced by a single model call. Every event
	// between model_started and the next model_started (or turn_finished) carries
	// the same invocation_id, so clients can unambiguously associate thinking,
	// tool calls, and compaction events with their parent model invocation
	// without relying on implicit temporal ordering.
	InvocationID string

	// Tool events (ToolStarted / ToolFinished / Observed). For EventObserved,
	// Observation carries the one-line summary and Failure the FailureType.
	//
	// CallID is the model's tool_call id (the loop fills a fallback when the
	// provider omits one, so it is always set). It is the STABLE identity of a
	// single tool call — the same across this call's ToolStarted, Observed, and
	// ToolFinished — so a UI keys one tool card on it and updates in place
	// (running → completed) instead of appending a new card per event. Unlike
	// event_id (a per-send transport token) it is durable and replay-stable.
	CallID          string
	Step            int
	ToolName        string
	ToolArgs        string
	Observation     string
	Output          json.RawMessage         // EventToolFinished: structured tool-specific output side-channel
	Assets          []assets.Ref            // EventToolFinished: normalized clickable assets side-channel
	ToolUsage       *tools.ToolUsage        // EventToolFinished: managed-tool billing receipt; nil for local tools
	TextAnnotations []assets.TextAnnotation // EventTurnFinished: assistant-text ranges linked to assets
	Chunk           string                  // stdout/stderr chunk (ToolStdout / ToolStderr)
	Failure         string                  // EventObserved: the classified FailureType (e.g. "compile")
	Version         string                  // EventSkillLoaded: the loaded skill's version (name is in ToolName)
	SkillSource     string                  // EventSkillLoaded: "global" or "project" — where the skill came from

	// Executor declares which side executes this tool call. Empty or "server"
	// means the server executes it locally. "client" means the client must
	// execute it and deliver the result back via a tool_result message.
	Executor string

	// Todos carries the model's current task checklist on EventTodoUpdated (8.4).
	Todos []tools.Todo

	// Model / thinking.
	Text               string        // reasoning delta/snapshot or final answer, selected by Kind
	PromptTokens       int           // ModelFinished: current invocation context size
	CompletionTokens   int           // ModelFinished: current invocation output
	TotalTokens        int           // ModelFinished: current invocation provider total
	BillingUnits       int64         // ModelFinished: invocation Units; TurnFinished/Failed: total turn Units
	ModelBillingUnits  int64         // TurnFinished/Failed: cumulative model Gateway Usage Units
	ToolBillingUnits   int64         // TurnFinished/Failed: cumulative managed-tool Usage Units
	ExecutedToolCalls  int           // TurnFinished/Failed: calls that reached an executor
	SucceededToolCalls int           // TurnFinished/Failed: executed calls without an execution error
	BillableToolCalls  int           // TurnFinished/Failed: unique calls carrying a Gateway usage receipt
	Elapsed            time.Duration // ModelFinished: how long the model call took (P3.8 uses this)

	// Compaction (Compacted / ContextPruned). AfterTokens == 0 means "just
	// compacted, size not yet measured"; > 0 means the next model call measured
	// the reclaimed size. Ineffective marks a measured compaction that failed to
	// land back under the threshold (P12.b) — the session is cooling down and a
	// renderer should surface it as a warning, not silence.
	BeforeTokens int
	AfterTokens  int
	SavedTokens  int
	SummaryChars int
	Ratio        float64
	Ineffective  bool

	// ExitCode carries JobFinished's process exit code (P8.7). Zero for a
	// successful exit — omitted on the wire, where Text=="exited" already means
	// success; -1 for a start failure or signal kill.
	ExitCode int

	// Set when a step errored.
	Err string

	// ErrorCode classifies a terminal turn failure for hosts. It is deliberately
	// an open string set: transports expose it as error.code, while clients use
	// unknown values as a generic failure.
	ErrorCode string
}

// Emitter receives loop events. Implementations render (REPL), stream (live UI),
// or persist them. The Runner's emit helper is nil-safe, so an emitter is
// optional.
type Emitter interface {
	Emit(Event)
}
