package agent

import (
	"encoding/json"
	"sort"
	"time"

	"code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

// EventKind identifies what happened in the loop. Events are the agent runtime's
// event stream: the loop EMITS them and a subscriber (the REPL console today, a
// live-progress UI or a persisted event bus tomorrow) renders them. The loop
// itself never writes to stdout — that decoupling is what lets the same run feed
// a plain terminal, a "Thinking… 5s" ticker, or a remote UI unchanged.
type EventKind string

const (
	EventTurnAccepted  EventKind = "turn_accepted"
	EventTurnQueued    EventKind = "turn_queued"
	EventTurnStarted   EventKind = "turn_started"
	EventModelStarted  EventKind = "model_started"  // about to call the model
	EventModelRequest  EventKind = "model_request"  // the request envelope: model, advertised tools, context shape
	EventModelFinished EventKind = "model_finished" // model returned (carries latency)
	// EventModelRetrying: a model call failed with a transient error and the
	// resilience layer is about to retry it (or to replay a failed stream
	// without streaming). Ephemeral — live-only progress, like the token and
	// reasoning deltas: the durable record of the failure is model_finished's
	// err / turn_failed, and the telemetry store's per-request attempts/trace.
	// A client renders it as a transient "retrying (2/5) in 3s" notice and
	// clears it when the invocation resolves.
	EventModelRetrying  EventKind = "model_retrying"
	EventTokenDelta     EventKind = "token_delta"     // streamed final-answer text; ephemeral, not persisted
	EventReasoningDelta EventKind = "reasoning_delta" // streamed provider-visible reasoning; ephemeral, not persisted
	EventThinking       EventKind = "thinking"        // complete provider-visible reasoning snapshot; persisted
	// EventAssistantText is intermediate assistant narration emitted when the
	// model says something and then calls tools in the same step. It is
	// persisted so clients can render the full conversation — the model's
	// between-tool remarks — not just the final answer (turn_finished).
	EventAssistantText EventKind = "assistant_text"
	EventToolStarted   EventKind = "tool_started"
	EventToolStdout    EventKind = "tool_stdout" // a stdout chunk during tool execution
	EventToolStderr    EventKind = "tool_stderr" // a stderr chunk during tool execution
	EventToolFinished  EventKind = "tool_finished"
	EventObserved      EventKind = "observed"      // a tool result was classified (P4.1)
	EventAutoApproved  EventKind = "auto_approved" // auto mode granted a side-effecting call without a human prompt (audit; p9.1 §12.3)
	EventReflected     EventKind = "reflected"     // a finalize self-check fired (P4.3)
	EventPreMutation   EventKind = "pre_mutation"  // a pre-mutation root-cause self-check fired (P4.3-R Move 3)
	EventVerified      EventKind = "verified"      // a deterministic finalize verify ran (P4.3-R Move 2)
	EventSkillLoaded   EventKind = "skill_loaded"  // a skill body was loaded (P6)
	EventTodoUpdated   EventKind = "todo_updated"  // the model's task checklist changed (8.4)
	EventCompacted     EventKind = "compacted"
	// EventContextPruned: tier-0 deterministic pruning ran (P12.c) — old tool
	// results truncated / think-blocks stripped outside the protected tail, with
	// no LLM call. SavedTokens carries the approximate reclaimed size; the true
	// size is measured by the next model call, like compaction.
	EventContextPruned EventKind = "context_pruned"
	// EventContextEdited: pre-compaction context editing cleared stale tool
	// results (no LLM call). Text carries the count cleared.
	EventContextEdited EventKind = "context_edited"
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

	// Plan mode (10.1). PlanStateChanged fires whenever the plan state machine
	// transitions — entering plan mode (enter_plan_mode), proposing, approving,
	// rejecting, or exiting — so a client can render plan-mode state without
	// polling. PlanState carries the new state. PlanProposed fires when the model
	// calls propose_plan. Text carries the full plan content.
	// PlanApproved/PlanRejected fire after the user's verdict; Text carries the
	// plan ID.
	EventPlanStateChanged EventKind = "plan_state_changed"
	EventPlanProposed     EventKind = "plan_proposed"
	EventPlanApproved     EventKind = "plan_approved"
	EventPlanRejected     EventKind = "plan_rejected"

	// Human-in-the-Loop task clarification. AskUserPosted fires when the model
	// calls ask_user with a question and options. AskUserResolved fires when the
	// user answers. AskUserTimeout fires when no user is available (headless,
	// auto mode) and the tool returns its fallback message.
	EventAskUserPosted   EventKind = "ask_user_posted"
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

	// Embedded Flux workflow lifecycle. The Workflow payload carries topology,
	// canonical flux-workflow Task/Node transitions and terminal output. Task
	// and Node status values remain an open set owned by flux-workflow/domain.
	EventWorkflowStarted          EventKind = "workflow_started"
	EventWorkflowPlanReady        EventKind = "workflow_plan_ready"
	EventWorkflowTaskStarted      EventKind = "workflow_task_started"
	EventWorkflowTaskStateChanged EventKind = "workflow_task_state_changed"
	EventWorkflowNodeStateChanged EventKind = "workflow_node_state_changed"
	EventWorkflowTaskProgress     EventKind = "workflow_task_progress"
	EventWorkflowNodeProgress     EventKind = "workflow_node_progress"
	EventWorkflowNodeDebug        EventKind = "workflow_node_debug"
	EventWorkflowToolProgress     EventKind = "workflow_tool_progress"
	EventWorkflowToolLog          EventKind = "workflow_tool_log"
	EventWorkflowToolStream       EventKind = "workflow_tool_stream"
	EventWorkflowToolStreamEnd    EventKind = "workflow_tool_stream_end"
	EventWorkflowFanoutProgress   EventKind = "workflow_fanout_progress"
	EventWorkflowTaskSucceeded    EventKind = "workflow_task_succeeded"
	EventWorkflowTaskFailed       EventKind = "workflow_task_failed"
	EventWorkflowTaskSuspended    EventKind = "workflow_task_suspended"
	EventWorkflowFinished         EventKind = "workflow_finished"
	EventWorkflowFailed           EventKind = "workflow_failed"
	EventWorkflowSuspended        EventKind = "workflow_suspended"
)

// Event is a single point in a turn — a discriminated union where Kind selects
// which fields are meaningful. It is plain data (no behavior, no pointers into
// runtime state) so it can be rendered, logged, or sent over a wire unchanged.
// DiffFile is one file's contribution to a turn's diffstat. The shape is defined
// in tools because that is the producing side (tools may not import agent); this
// alias keeps the event, the wire frame, and tools.ToolResult on one type.
type DiffFile = tools.FileWrite

// TurnDiffStat is what one turn wrote, accumulated across every successful write
// call in the turn — including writes performed inside workflow nodes, which are
// bubbled up through tools.ToolResult.NestedWrites.
//
// The counts are per-call sums, not net changes: a file edited three times adds
// up all three edits. That is deliberate (see docs/turn-diffstat-v1.md §3) —
// netting would need a snapshot of every file at turn start, and the workspace is
// not guaranteed to be a git repository.
type TurnDiffStat struct {
	Files        []DiffFile `json:"files"`
	FilesChanged int        `json:"files_changed"`
	AddedLines   int        `json:"added_lines"`
	RemovedLines int        `json:"removed_lines"`

	// index maps Files[i].Path to i so repeated writes to one path merge in O(1)
	// instead of rescanning. Never serialized.
	index map[string]int
}

// Add folds one write report into the turn. A path seen more than once sums its
// line counts and keeps the most "origin-like" status: a file created and then
// edited stays added, and any deletion wins.
func (d *TurnDiffStat) Add(w tools.FileWrite) {
	if w.Path == "" {
		return
	}
	if d.index == nil {
		d.index = make(map[string]int, len(d.Files))
		for i, f := range d.Files {
			d.index[f.Path] = i
		}
	}
	if i, ok := d.index[w.Path]; ok {
		f := &d.Files[i]
		f.Added += w.Added
		f.Removed += w.Removed
		f.Status = mergeWriteStatus(f.Status, w.Status)
	} else {
		d.index[w.Path] = len(d.Files)
		d.Files = append(d.Files, w)
	}
	d.FilesChanged = len(d.Files)
	d.AddedLines += w.Added
	d.RemovedLines += w.Removed
}

// Empty reports whether the turn wrote nothing (the field is then omitted
// entirely instead of shipping a zeroed object).
func (d *TurnDiffStat) Empty() bool {
	return d == nil || len(d.Files) == 0
}

// Limited returns a copy truncated to at most maxFiles entries, largest first by
// added+removed. The three summary numbers keep their full-turn values, so a
// clipped list never shrinks files_changed. The receiver is not modified.
func (d *TurnDiffStat) Limited(maxFiles int) *TurnDiffStat {
	if d.Empty() {
		return nil
	}
	out := &TurnDiffStat{
		Files:        append([]DiffFile(nil), d.Files...),
		FilesChanged: d.FilesChanged,
		AddedLines:   d.AddedLines,
		RemovedLines: d.RemovedLines,
	}
	if maxFiles > 0 && len(out.Files) > maxFiles {
		sort.SliceStable(out.Files, func(i, j int) bool {
			return out.Files[i].Added+out.Files[i].Removed > out.Files[j].Added+out.Files[j].Removed
		})
		out.Files = out.Files[:maxFiles]
	}
	return out
}

func mergeWriteStatus(old, next string) string {
	if old == tools.WriteDeleted || next == tools.WriteDeleted {
		return tools.WriteDeleted
	}
	if old == tools.WriteAdded || next == tools.WriteAdded {
		return tools.WriteAdded
	}
	return tools.WriteModified
}

type Event struct {
	Kind EventKind
	At   time.Time

	// Correlation IDs: which session and turn produced this event. A single
	// console reads them as constant, but a multiplexed bus (concurrent runs, a
	// web UI, DreamAI) needs them to keep streams from crossing.
	SessionID              string
	TurnID                 string
	RequestID              string                      // client-generated idempotency/correlation key for agent_input
	ValidationContract     *session.ValidationContract `json:"validation_contract,omitempty"`
	ValidationContractHash string                      `json:"validation_contract_hash,omitempty"`
	UserAssets             []model.GatewayAssetRef
	LocalAssets            []model.LocalAssetRef
	QueuePosition          int    // EventTurnQueued: one-based scheduler position
	QueueReason            string // EventTurnQueued: global_capacity | session_serialization

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
	Workflow        json.RawMessage         // workflow_*: structured DAG/task/node payload
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

	// PlanState carries the new state on EventPlanStateChanged (10.1).
	PlanState PlanStatus

	// Model / thinking.
	ModelSelection     *model.Selection
	ModelSucceeded     *bool         // call outcome; does not imply tool/test/task success
	Text               string        // reasoning delta/snapshot or final answer, selected by Kind
	PromptTokens       int           // ModelFinished: current invocation context size
	CompletionTokens   int           // ModelFinished: current invocation output
	TotalTokens        int           // ModelFinished: current invocation provider total
	CachedPromptTokens int           // ModelFinished: prompt tokens served from the provider cache (a breakdown of PromptTokens; 0 when unreported)
	BillingUnits       int64         // ModelFinished: invocation Units; TurnFinished/Failed: total turn Units
	ModelBillingUnits  int64         // TurnFinished/Failed: cumulative model Gateway Usage Units
	ToolBillingUnits   int64         // TurnFinished/Failed: cumulative managed-tool Usage Units
	ExecutedToolCalls  int           // TurnFinished/Failed: calls that reached an executor
	SucceededToolCalls int           // TurnFinished/Failed: executed calls without an execution error
	BillableToolCalls  int           // TurnFinished/Failed: unique calls carrying a Gateway usage receipt
	Elapsed            time.Duration // ModelFinished: how long the model call took (P3.8 uses this)

	// DiffStat is what this turn wrote, filled only on the terminal kinds
	// (turn_finished, turn_failed, turn_cancelled). It is nil when nothing was
	// written, and it is deliberately absent from turn_paused: a paused turn
	// resumes and re-emits turn_finished later, so a half-turn stat here would
	// show the user two different numbers for the same turn.
	DiffStat *TurnDiffStat

	// ModelRetrying: the retry the resilience layer is about to make. Attempt is
	// the 1-based ordinal of the attempt that just failed, MaxAttempts the total
	// allowed, RetryDelayMs the backoff before the next attempt. RetryFallback
	// marks the stream→non-stream replay boundary instead of a timed retry
	// (Attempt/MaxAttempts are 0; RetryDelayMs may carry the provider's wait).
	Attempt       int
	MaxAttempts   int
	RetryDelayMs  int
	RetryFallback bool

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

	// CancelledReason is set on turn_cancelled events. Currently always
	// "user_requested" (the client sent cancel_turn or agent_input
	// kind=command text=cancel). A future "client_disconnected" value
	// will distinguish WebSocket-loss cancellation when that behaviour
	// is implemented.
	CancelledReason string

	// ModelRequest carries the request envelope on EventModelRequest — the
	// facts a trajectory view needs to show WHAT was asked of the model, not
	// just that a call happened. ToolNames lists the advertised tools in
	// advertisement order; MessageCount/SystemPromptChars/ToolsPromptChars
	// describe the context shape without duplicating its (large) content.
	ModelName         string
	Provider          string
	BaseURL           string
	ToolNames         []string
	MessageCount      int
	SystemPromptChars int
	ToolsPromptChars  int
	Streamed          bool
	Temperature       float64
	ReasoningEffort   string
	ToolChoice        string
}

// Emitter receives loop events. Implementations render (REPL), stream (live UI),
// or persist them. The Runner's emit helper is nil-safe, so an emitter is
// optional.
type Emitter interface {
	Emit(Event)
}
