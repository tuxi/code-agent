package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

type Runner struct {
	Model       model.Provider
	ModelName   string
	Temperature float64
	Tools       *tools.Registry
	MaxSteps    int

	// MaxWebSearches caps web_search calls within one user turn (0 = default).
	// A search-happy model that keeps reformulating instead of answering gets a
	// "stop searching" result past the cap; the counter resets each turn.
	MaxWebSearches int

	// Approver gates side-effecting tool calls. If nil, side-effecting tools are
	// denied (see approve()). Read-only tools never consult it.
	Approver Approver

	// ClientWaiter blocks the turn goroutine while a client-executed tool runs.
	// When nil (no client connected, or headless mode), all tools run server-side.
	// v1.1: see docs/protocols/agent-wire-v1.1-client-tool-execution.md §5.
	ClientWaiter ClientToolWaiter

	// ClientToolTimeout is the lease timeout for a single client tool call.
	// Zero uses a 2-minute default.
	ClientToolTimeout time.Duration

	// Observer enriches each tool result into a structured Observation (P4.1).
	// Nil-safe: when unset, raw tool results are appended unchanged.
	Observer Observer

	// Reflector runs a one-shot self-check at the finalize boundary (P4.3).
	// Nil-safe: when unset, the model's first "done" is accepted as before.
	Reflector Reflector

	// Hook runs user-configured pre/post-tool commands (8.5). Nil-safe: when unset,
	// tools run exactly as before.
	Hook ToolHook

	// Stream, when true AND the provider supports it, streams the model's text as
	// it generates (8.6) — emitting EventTokenDelta for a renderer to show live.
	// The returned Response is identical to the non-streamed one, so the loop is
	// unaffected. Set by the TUI; run/repl leave it off.
	Stream bool

	// RemindSkills, when true, injects a one-shot ephemeral reminder on the first
	// model call of each turn to check the Skills list and load a matching skill
	// (P6). It makes skill-loading consistent across models rather than depending
	// on a model's agency. Set by the CLI when the project has skills.
	RemindSkills bool

	// PlanApprover handles plan-level approval (plan → approve → execute).
	// When nil, propose_plan auto-approves (test/headless path). Set by the
	// REPL, TUI, or server to gate plan execution behind a human decision.
	PlanApprover PlanApprover

	// PlanState tracks the planning workflow phase. Exported so the TUI and REPL
	// can toggle plan mode manually (Ctrl+P, /plan).
	PlanState PlanStatus

	// activePlan is the current plan, populated when propose_plan is called.
	activePlan *Plan

	// planTitle is set by enter_plan_mode's input or /plan command.
	planTitle string

	// PlanTools is the restricted toolset used during Planning/Proposing states.
	// Built from planModeToolNames at construction time. Exported so the cmd
	// layer can wire it from planModeToolNames after runner construction.
	PlanTools *tools.Registry

	// lastThinking stores the model's most recent thinking text (the response
	// content, or the live-streamed text when the call failed) so propose_plan
	// can extract the plan body without duplicating it in args.
	lastThinking string

	Compactor session.Compactor

	// Checkpointer, if set, persists the session mid-turn at each consistent loop
	// boundary (v1.2 §2). Best-effort: a checkpoint error never fails the turn — the
	// caller's turn-boundary Save is the backstop. Set by the serve/embedded path
	// (which holds the repository); CLI/TUI leave it nil and save only at the
	// turn boundary as before.
	Checkpointer Checkpointer

	// Emitter, if set, receives the turn's event stream (thinking, tool calls,
	// compaction, model latency). The loop emits; it never writes to stdout
	// itself, so the UI is fully decoupled from the runtime.
	Emitter Emitter

	// WorkspaceRoot is the absolute project root directory for this runner.
	// It is set at construction and used to build ExecutionContext for each
	// tool call. For the serve path, this comes from the WorkspaceInstance;
	// for REPL/TUI, from cfg.Workspace.Root.
	WorkspaceRoot string

	// Correlation IDs stamped onto every emitted event. Set per RunTurn (which is
	// sequential on a Runner), so an event always carries which session and turn
	// produced it.
	emitSessionID    string
	emitTurnID       string
	emitInvocationID string // set at the start of each model call; stamped on all events
}

// invocationSeq backs per-turn invocation ids; monotonically increases within a
// single process. Combined with turn_id on the wire, clients get globally unique
// (session, turn, invocation) triples.
var invocationSeq atomic.Uint64

func newInvocationID() string { return fmt.Sprintf("inv_%d", invocationSeq.Add(1)) }

// nextSessionTurnID returns a session-scoped, monotonic turn identifier. Unlike
// a process-global counter (which resets on restart and can produce duplicates
// within a resumed session), the counter lives in sess.Metadata — it is persisted
// with the session and survives process restarts, so every turn_id is unique
// within the conversation forever.
func nextSessionTurnID(sess *session.Session) string {
	n := 0
	switch v := sess.Metadata["turn_seq"].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	}
	n++
	sess.Metadata["turn_seq"] = float64(n) // float64 for JSON round-trip
	return fmt.Sprintf("turn_%d", n)
}

// emit sends an event to the configured Emitter, if any. Nil-safe.
func (r *Runner) emit(e Event) {
	if r.Emitter == nil {
		return
	}
	e.At = time.Now()
	e.SessionID = r.emitSessionID
	e.TurnID = r.emitTurnID
	e.InvocationID = r.emitInvocationID
	r.Emitter.Emit(e)
}

// TurnResult is the outcome of a single turn: the final answer the model
// produced and the tool steps taken to get there. The conversation itself lives
// on the Session, which accumulates across turns.
type TurnResult struct {
	Final        string
	Steps        []Step
	PromptTokens int

	// TokensUsed is the turn's CUMULATIVE token consumption: the sum over every
	// model call this turn of prompt+completion usage. It differs from
	// PromptTokens on purpose — PromptTokens is a GAUGE (the last call's context
	// size, which drives compaction), TokensUsed is a COUNTER (what a turn-budget
	// must accumulate). Summing PromptTokens across turns would conflate context
	// size with spend; a /goal budget reads TokensUsed.
	TokensUsed int

	// HitStepLimit is true when the turn exhausted MaxSteps and Final came from the
	// best-effort tool-free answer rather than the model finishing on its own. A
	// caller that delegates a turn (the subagent, 8.3) uses it to avoid passing off
	// a non-convergent run as a clean conclusion.
	HitStepLimit bool
}

const defaultMaxSteps = 24

// webSearchToolName is the search tool subject to the per-turn budget below.
const webSearchToolName = "web_search"

// defaultMaxWebSearches caps web_search calls per user turn. Search-happy models
// reformulate the same query many ways instead of answering; the cap forces them
// to stop and use what they have. Set above a typical real need so it only bites
// genuine thrash.
const defaultMaxWebSearches = 5

// toolCallSeq backs synthetic tool_call ids (see RunTurn). Process-global and
// monotonic so synthesized ids never collide within a session.
var toolCallSeq atomic.Uint64

func nextCallID() string {
	return fmt.Sprintf("call_%d", toolCallSeq.Add(1))
}

// RunTurn runs one turn of the agent against a persistent session: it appends
// the user's input to the session history, then drives the uniform loop —
// call the model (with tool schemas); if it returns no tool calls, that text is
// the final answer; otherwise execute every tool call, append each result to
// the session, and loop — until a final answer or the per-turn step limit.
//
// The session's Messages survive the call, so the next turn sees this turn's
// full history. The loop contains no per-tool logic and no decision state
// machine: the model owns control flow, the runtime executes and gates tools.
func (r *Runner) RunTurn(ctx context.Context, sess *session.Session, userInput string) (TurnResult, error) {
	if r.Model == nil {
		return TurnResult{}, errors.New("missing model provider")
	}
	if r.Tools == nil {
		r.Tools = tools.NewRegistry()
	}
	if r.MaxSteps <= 0 {
		r.MaxSteps = defaultMaxSteps
	}

	r.emitSessionID = sess.ID
	r.emitTurnID = nextSessionTurnID(sess)
	r.emitInvocationID = "" // cleared each turn; set per model call

	// Append the user's turn to the persistent session history.
	sess.Messages = append(sess.Messages, model.Message{
		Role:    model.RoleUser,
		Content: userInput,
	})
	sess.UpdatedAt = time.Now()
	r.emit(Event{Kind: EventTurnStarted, Text: userInput})

	var turn TurnResult

	// Reflection (P4.3) per-turn state: at most one self-check pass, and the
	// ephemeral nudge to apply on the next request once it fires.
	reflected := false
	pendingReflection := ""

	// Per-turn web_search budget. Counts continuously across this turn (it resets
	// when Run returns and the next user turn starts a fresh counter). A
	// search-happy model reformulating the same query is cut off past the cap.
	webSearches := 0

	for i := 0; i < r.MaxSteps; i++ {
		// A canceled context (Ctrl-C) must stop the turn at the step boundary
		// without waiting for the next HTTP call to time out.
		if err := ctx.Err(); err != nil {
			return turn, err
		}
		// Compact before each model call, not just at the turn boundary: a single
		// turn with many tool calls can grow the prompt past the threshold mid-loop.
		if err := r.maybeCompact(ctx, sess); err != nil {
			return turn, err
		}

		// Recompute the toolset each iteration: plan mode can be entered or
		// exited mid-turn by enter_plan_mode / propose_plan, so the tool list
		// must reflect the current planState.
		activeTools := r.Tools
		if (r.PlanState == PlanStatusPlanning || r.PlanState == PlanStatusProposing) && r.PlanTools != nil {
			activeTools = r.PlanTools
		}
		toolDefs := toolDefinitions(activeTools)
		advertised := make(map[string]bool, len(toolDefs))
		for _, d := range toolDefs {
			advertised[d.Function.Name] = true
		}

		// Convergence nudge: once a turn has made many tool calls, steer a model
		// that lacks agentic restraint toward answering. The nudge is ephemeral —
		// appended only to this request, never persisted — so it keeps applying
		// pressure without polluting history. (max_steps still backstops it.)
		msgs := sess.Messages
		if n := len(turn.Steps); n >= r.nudgeThreshold() {
			msgs = withConvergenceNudge(sess.Messages, n)
		}
		// Reflection nudge (P4.3): apply the self-check once, ephemerally — the
		// same non-persisted mechanism as the convergence nudge, fired at the
		// opposite boundary (about to finish, not over-investigating).
		if pendingReflection != "" {
			msgs = appendEphemeralUser(msgs, pendingReflection)
			pendingReflection = ""
		}
		// Skills reminder (P6): on the first model call of a turn, remind the model
		// to load a matching skill. Ephemeral, and the model still decides — this
		// makes skill-loading consistent across models instead of depending on a
		// model's agency to act on the index unprompted.
		if i == 0 && r.RemindSkills {
			msgs = appendEphemeralUser(msgs, skillsReminder)
		}
		// Plan mode (read-only): steer the model to produce a plan, not changes. The
		// read-only toolset already prevents edits; this shapes the output.
		// Plan mode: when in Planning state, inject the planning guidance prompt.
		// The restricted toolset already prevents edits; this shapes the output.
		if r.PlanState == PlanStatusPlanning {
			msgs = appendEphemeralUser(msgs, planningPrompt)
		}

		r.emitInvocationID = newInvocationID()
		r.emit(Event{Kind: EventModelStarted})
		modelStart := time.Now()
		var streamed strings.Builder
		resp, err := r.complete(ctx, model.Request{
			Model:       r.ModelName,
			Temperature: r.Temperature,
			Messages:    msgs,
			Tools:       toolDefs,
		}, &streamed)
		// Capture the model's text for plan extraction and thinking events.
		// Prefer the complete response, but fall back to what was streamed live
		// when the call failed before a Response could be assembled (e.g. a
		// mid-stream timeout) — otherwise the thinking shown live would be lost
		// from the persisted event log on a failed turn.
		thinking := resp.Content
		if thinking == "" {
			thinking = strings.TrimSpace(streamed.String())
		}
		// Emit EventThinking BEFORE EventModelFinished so the logical event
		// order is model_started → thinking → model_finished, and the at
		// timestamp reflects when thinking was produced, not when the turn
		// finished processing it.
		if thinking != "" {
			r.lastThinking = thinking
		}
		// Persist thinking when the model also requested tools (a finishing turn's
		// text is persisted as TurnFinished instead, so emitting here would
		// duplicate it), OR when the call failed — a failed turn produces no
		// TurnFinished, so this is the only chance to retain the live thinking.
		if thinking != "" && (resp.HasToolCalls() || err != nil) {
			r.emit(Event{Kind: EventThinking, Text: thinking})
		}
		// Always emit ModelFinished, even on error: it pairs with ModelStarted, so
		// a live renderer's "Thinking…" ticker is always stopped (no leaked timer).
		r.emit(Event{
			Kind:         EventModelFinished,
			PromptTokens: resp.Usage.PromptTokens,
			Elapsed:      time.Since(modelStart),
			Err:          errString(err),
		})
		if err != nil {
			return turn, err
		}

		turn.PromptTokens = resp.Usage.PromptTokens
		turn.TokensUsed += resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		sess.PromptTokens = resp.Usage.PromptTokens
		// This call's prompt size is the true post-compaction size if a compaction
		// just ran, so finalize its observability stat here.
		if stat := sess.FinalizeCompaction(resp.Usage.PromptTokens); stat != nil {
			r.emit(Event{
				Kind:         EventCompacted,
				BeforeTokens: stat.BeforeTokens,
				AfterTokens:  stat.AfterTokens,
				SavedTokens:  stat.SavedTokens,
				Ratio:        stat.CompressionRatio,
				SummaryChars: stat.SummaryChars,
			})
		}

		// Some OpenAI-compatible providers occasionally return a tool call with
		// an empty id. Assign a stable, unique id here so the echoed assistant
		// message and the tool result messages reference the SAME non-empty
		// tool_call_id — otherwise the API rejects the next request with
		// "insufficient tool messages following tool_calls message".
		for j := range resp.ToolCalls {
			if resp.ToolCalls[j].ID == "" {
				resp.ToolCalls[j].ID = nextCallID()
			}
		}

		// No tool calls => the model wants to finish.
		if !resp.HasToolCalls() {
			// Reflection (P4.3): before accepting "done", run one grounded
			// self-check. If the turn's work looks unfinished — a test edited to go
			// green, a change left unverified — re-prompt with an ephemeral nudge
			// and do NOT persist this premature finish (persisting it would leave a
			// retracted answer, and two assistant turns in a row, in history).
			// One-shot per turn; the model decides whether to actually do more.
			if r.Reflector != nil && !reflected {
				rc := r.Reflector.Reflect(turn.Steps)
				if nudge := rc.Nudge(); nudge != "" {
					reflected = true
					pendingReflection = nudge
					r.emit(Event{Kind: EventReflected, Text: nudge})
					continue
				}
			}
			sess.Messages = append(sess.Messages, resp.AssistantMessage())
			sess.UpdatedAt = time.Now()
			turn.Final = resp.Content
			r.emit(Event{Kind: EventTurnFinished, Text: turn.Final})
			return turn, nil
		}

		// Tool-call path: the assistant turn must precede its tool results in
		// history (the API requires the tool_calls message before the answers).
		sess.Messages = append(sess.Messages, resp.AssistantMessage())
		sess.UpdatedAt = time.Now()

		// Execute EVERY requested tool call. Each one must get a tool result
		// with a matching tool_call_id, or the next request will be rejected.
		//
		// If the context is cancelled mid-batch (Ctrl-C, or an iOS Suspend), we do
		// NOT bail with a dangling assistant message: the tool_calls message above
		// references every call in resp.ToolCalls, so leaving some without a result
		// would persist an inconsistent history the provider rejects on resume
		// ("insufficient tool messages following tool_calls"). Instead we append a
		// synthetic interrupted-result for this call and every one after it, keeping
		// the batch balanced, then return. On resume the model sees they did not run
		// and re-issues what it still needs (v1.2 §2.2).
		var cancelErr error
		for idx, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				cancelErr = err
				for _, rem := range resp.ToolCalls[idx:] {
					sess.Messages = append(sess.Messages, model.Message{
						Role:       model.RoleTool,
						ToolCallID: rem.ID,
						Content:    toolInterruptedObservation,
					})
				}
				sess.UpdatedAt = time.Now()
				break
			}
			input := json.RawMessage(call.Function.Arguments)

			step := Step{
				Index:     len(turn.Steps) + 1,
				ToolName:  call.Function.Name,
				ToolInput: input,
				StartedAt: time.Now(),
			}
			tool, known := activeTools.Get(call.Function.Name)
			valid := advertised[call.Function.Name] && known
			executor := r.executorFor(tool, known)
			r.emit(Event{
				Kind:     EventToolStarted,
				CallID:   call.ID,
				Step:     step.Index,
				ToolName: call.Function.Name,
				ToolArgs: call.Function.Arguments,
				Executor: executor,
			})

			// Pre-tool hook (8.5): a configured command may block the call. Only
			// consulted for a real tool, so an unknown call still reports plainly.
			var blockReason string
			if valid && executor != "client" {
				blockReason = r.preHookBlock(ctx, call.Function.Name, input)
			}

			// Per-turn web_search budget: count every search call, then refuse
			// further ones past the cap so a search-happy model stops reformulating
			// and answers with what it has.
			if call.Function.Name == webSearchToolName {
				webSearches++
			}

			var observation string
			var execErr error
			toolStart := time.Now()
			switch {
			case !valid:
				execErr = fmt.Errorf("unknown tool: %s", call.Function.Name)
			case call.Function.Name == webSearchToolName && webSearches > r.maxWebSearches():
				observation = fmt.Sprintf(
					"Search budget reached: %d web searches already this turn (limit %d). "+
						"Stop searching — reformulating the query will not surface new results. "+
						"Answer with the results you already have, or web_fetch a specific URL.",
					webSearches-1, r.maxWebSearches())
			case blockReason != "":
				observation = "The tool call was blocked by a configured hook. " + blockReason
			case tools.HasSideEffectsFor(tool, input) && !r.approve(call.Function.Name, input):
				observation = "The user declined to run this tool. No changes were made."
			case executor == "client":
				result, waitErr := r.ClientWaiter.Wait(ctx, call.ID, r.clientToolTimeout())
				if waitErr != nil {
					execErr = waitErr
				} else if result.IsError {
					execErr = fmt.Errorf("%s", result.Content)
				} else {
					observation = result.Content
				}
			default:
				observation, execErr = r.executeTool(ctx, tool, call.ID, input)
				if execErr == nil {
					// Post-tool hook (8.5): react to the change (format/lint). It runs
					// the configured command but does not alter the result in v1.
					r.postHook(ctx, call.Function.Name, input, observation)
				}
			}
			if execErr != nil {
				step.Error = execErr.Error()
				observation = "Tool error: " + execErr.Error()
			}

			// Skill telemetry (P6): if the tool loaded a skill, emit a versioned
			// event. Interface-driven, so the loop stays tool-agnostic.
			if known && execErr == nil {
				if sa, ok := tool.(tools.SkillAnnouncer); ok {
					if name, ver, src, loaded := sa.AnnounceSkill(input); loaded {
						r.emit(Event{Kind: EventSkillLoaded, ToolName: name, Version: ver, SkillSource: src})
					}
				}
				// Todo checklist (8.4): same interface-driven pattern — the loop emits
				// the updated list without knowing the tool by name.
				if ta, ok := tool.(tools.TodoAnnouncer); ok {
					if todos, ok := ta.AnnounceTodos(input); ok {
						r.emit(Event{Kind: EventTodoUpdated, Todos: todos})
					}
				}
			}

			// Enrich the raw result into a structured Observation (P4.1). Observe
			// runs on the *full* output so salient lines survive truncation; the
			// body is then truncated and a failure/summary block prepended, so the
			// model sees the signal first. No-op when no Observer is set — the loop
			// stays neutral and tool-agnostic.
			if r.Observer != nil {
				obs := r.Observer.Observe(call.Function.Name, observation)
				observation = obs.Render(TruncateObservation(observation, 9800))
				r.emit(Event{
					Kind:        EventObserved,
					CallID:      call.ID,
					Step:        step.Index,
					ToolName:    call.Function.Name,
					Observation: obs.Summary,
					Failure:     string(obs.FailureType),
				})
			} else {
				observation = TruncateObservation(observation, 9800)
			}

			step.Observation = observation
			step.FinishedAt = time.Now()
			turn.Steps = append(turn.Steps, step)

			r.emit(Event{
				Kind:        EventToolFinished,
				CallID:      call.ID,
				Step:        step.Index,
				ToolName:    call.Function.Name,
				Observation: observation,
				Elapsed:     time.Since(toolStart),
				Err:         step.Error,
			})

			sess.Messages = append(sess.Messages, model.Message{
				Role:       model.RoleTool,
				ToolCallID: call.ID,
				Content:    observation,
			})
			sess.UpdatedAt = time.Now()
		}
		// The batch was cut short by cancellation; history is now balanced (every
		// call has a result), so returning leaves a resumable session.
		if cancelErr != nil {
			return turn, cancelErr
		}

		// Checkpoint at this consistent boundary (v1.2 §2): the tool batch is
		// complete and history is balanced, so a hard process kill (iOS jetsam)
		// before the next model call loses at most the in-progress step, not the
		// whole turn. Best-effort — never fails the turn.
		r.checkpoint(ctx, sess)
	}

	// Step limit reached. Don't discard the work: the model has gathered tool
	// results in the history, so give it one final tool-free call to answer from
	// what it has — instead of a useless "stopped" message that forces the user to
	// re-ask (and re-pay for the whole investigation).
	final, finalTokens := r.finalAnswerAfterLimit(ctx, sess)
	turn.Final = final
	turn.TokensUsed += finalTokens
	turn.HitStepLimit = true
	r.emit(Event{Kind: EventTurnFinished, Text: turn.Final})
	return turn, nil
}

// nudgeThreshold is the tool-call count at which the convergence nudge starts —
// half the step budget, with a floor so short budgets don't nudge too eagerly.
func (r *Runner) nudgeThreshold() int {
	t := r.MaxSteps / 2
	if t < 6 {
		t = 6
	}
	return t
}

// maxWebSearches is the per-turn web_search budget (configurable, with a default).
func (r *Runner) maxWebSearches() int {
	if r.MaxWebSearches > 0 {
		return r.MaxWebSearches
	}
	return defaultMaxWebSearches
}

// skillsReminder is the ephemeral first-call nudge (P6). It is phrased to be
// safe across turns ("not already loaded") so a skill loaded in an earlier turn
// is not redundantly re-loaded.
const skillsReminder = "[reminder] Before you act: check the Skills list in the system prompt. " +
	"If this task matches a skill you have not already loaded, call load_skill(name) and follow " +
	"it first — that is reading project guidance, not extra investigation."

// planningPrompt is injected as an ephemeral user message when the model enters the
// Planning state (via enter_plan_mode or /plan). The restricted toolset already
// blocks project edits; this tells the model what to produce instead.
const planningPrompt = "[plan mode] You are in PLAN MODE. You can read, search, and write plan " +
	"files to .codeagent/plans/, but you CANNOT edit project files or run commands. " +
	"Research the task thoroughly, then produce a concrete implementation plan. " +
	"Your plan should include:\n" +
	"1. Problem summary — what needs to be done\n" +
	"2. Files to change — list each file and what changes\n" +
	"3. Approach — the implementation strategy and key design decisions\n" +
	"4. Step-by-step order — the sequence of changes\n" +
	"5. Risks and edge cases — what could go wrong and how to handle it\n" +
	"When your plan is complete, write it to .codeagent/plans/ and call propose_plan " +
	"to submit it for user approval. Do NOT make any project changes. " +
	"You may track your plan's steps with todo_write."

// withConvergenceNudge returns a copy of msgs with a transient reminder appended,
// steering the model to answer now instead of over-investigating.
func withConvergenceNudge(msgs []model.Message, toolCalls int) []model.Message {
	return appendEphemeralUser(msgs, fmt.Sprintf("[reminder] You have already made %d tool calls and very"+
		" likely have enough to answer. Unless you are genuinely blocked, stop calling tools and give your"+
		" final answer now. Do not re-run similar queries to double-check.", toolCalls))
}

// appendEphemeralUser returns a copy of msgs with a transient user message
// appended. Both the convergence nudge and the reflection nudge use it: the
// message shapes the next request only and is never persisted to the session.
func appendEphemeralUser(msgs []model.Message, content string) []model.Message {
	out := make([]model.Message, len(msgs), len(msgs)+1)
	copy(out, msgs)
	return append(out, model.Message{Role: model.RoleUser, Content: content})
}

const (
	stepLimitMessage = "Agent stopped: reached the step limit before finishing."
	stepLimitNudge   = "You've reached the step limit and cannot call more tools. Give your best final answer now, based on everything gathered so far."
)

// toolInterruptedObservation is the placeholder tool result written for a call
// that never ran because the turn was cancelled/suspended mid-batch. It keeps the
// assistant tool_calls message balanced (one result per call) so the persisted
// history is valid to resend, and tells the model the call did not execute so it
// re-issues if still needed (v1.2 §2.2).
const toolInterruptedObservation = "This tool call did not run: the turn was interrupted (app suspended or cancelled) before it executed. Re-issue the call if the result is still needed."

// finalAnswerAfterLimit makes one tool-free model call so the agent answers from
// what it already gathered when the step limit is hit. The nudge is ephemeral
// (not persisted); only the answer is appended to history, so the conversation
// continues cleanly.
// finalAnswerAfterLimit returns the best-effort answer AND the tokens that one
// call consumed, so the turn-level counter (TurnResult.TokensUsed) stays exact
// even on the step-limit path.
func (r *Runner) finalAnswerAfterLimit(ctx context.Context, sess *session.Session) (string, int) {
	if ctx.Err() != nil {
		return stepLimitMessage, 0
	}
	msgs := make([]model.Message, len(sess.Messages), len(sess.Messages)+1)
	copy(msgs, sess.Messages)
	msgs = append(msgs, model.Message{
		Role:    model.RoleUser,
		Content: stepLimitNudge,
	})

	r.emit(Event{Kind: EventModelStarted})
	start := time.Now()
	resp, err := r.complete(ctx, model.Request{
		Model:       r.ModelName,
		Temperature: r.Temperature,
		Messages:    msgs,
		// No Tools: the model must answer with text, not request more tools.
	}, nil)
	r.emit(Event{Kind: EventModelFinished, PromptTokens: resp.Usage.PromptTokens, Elapsed: time.Since(start), Err: errString(err)})
	tok := resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	// A leaked tool-call markup (deepseek, when forced to answer with no tools) is
	// not an answer — don't show the user noise or persist it; fall back cleanly.
	// The call still consumed tokens, so report them regardless.
	if err != nil || resp.Content == "" || LooksLikeToolCallLeak(resp.Content) {
		return stepLimitMessage, tok
	}

	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleAssistant, Content: resp.Content})
	sess.PromptTokens = resp.Usage.PromptTokens
	sess.UpdatedAt = time.Now()
	return resp.Content, tok
}

// Checkpointer persists the session mid-turn, at the consistent boundary after
// each completed loop iteration (v1.2 §2). It bounds the blast radius of a hard
// process kill (iOS jetsam) to the in-progress step, whereas the caller's
// turn-boundary Save is the backstop. Implementations must be crash-safe and
// SHOULD detach cancellation (persist even as the turn is being suspended).
type Checkpointer interface {
	Checkpoint(ctx context.Context, sess *session.Session) error
}

// checkpoint persists the session at a consistent boundary. Nil-safe and
// best-effort: a checkpoint failure is swallowed so it never fails the turn — the
// turn-boundary Save surfaces persistent errors.
func (r *Runner) checkpoint(ctx context.Context, sess *session.Session) {
	if r.Checkpointer == nil {
		return
	}
	_ = r.Checkpointer.Checkpoint(ctx, sess)
}

// maybeCompact compacts the session when it has grown past the token threshold.
// It is best-effort: a nil Compactor means the caller opted out.
//
// PromptTokens is deliberately NOT reset afterwards. The pre-compaction count is
// stale, but faking a 0-token state would hide a compaction that failed to get
// under the window. Instead the next model call (immediately after this, at the
// top of the loop) measures the true reduced size and refreshes PromptTokens —
// which also finalizes the observability stat. A compaction that changed nothing
// (the recent window already is the whole history) is not recorded.
func (r *Runner) maybeCompact(ctx context.Context, sess *session.Session) error {
	if r.Compactor == nil || !sess.NeedCompaction() {
		return nil
	}
	before := sess.PromptTokens
	prevLen, prevSummary := len(sess.Messages), sess.Summary
	if err := r.Compactor.Compact(ctx, sess); err != nil {
		return err
	}
	if len(sess.Messages) == prevLen && sess.Summary == prevSummary {
		return nil // nothing was folded
	}
	sess.RecordCompaction(before, len(sess.Summary))
	r.emit(Event{Kind: EventCompacted, BeforeTokens: before, SummaryChars: len(sess.Summary)})
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// complete calls the model, streaming its text live (8.6) when Stream is set and
// the provider supports it — emitting EventTokenDelta for each text delta. Either
// way it returns the same complete Response, so the loop's control flow is
// identical whether or not streaming happened.
// complete calls the model. When streaming, each text delta is mirrored into
// streamed (when non-nil) so the caller retains the live thinking text even if
// the call ultimately fails — the provider returns an empty Response on a mid-
// stream read error, discarding its own accumulation, so this is the only copy
// that survives a failed turn.
func (r *Runner) complete(ctx context.Context, req model.Request, streamed *strings.Builder) (model.Response, error) {
	if r.Stream {
		if sp, ok := r.Model.(model.StreamingProvider); ok {
			return sp.CompleteStream(ctx, req, func(delta string) {
				if streamed != nil {
					streamed.WriteString(delta)
				}
				r.emit(Event{Kind: EventTokenDelta, Text: delta})
			})
		}
	}
	return r.Model.Complete(ctx, req)
}

func (r *Runner) executeTool(ctx context.Context, tool tools.Tool, callID string, input json.RawMessage) (string, error) {
	ec := tools.ExecutionContext{
		WorkspaceRoot: r.WorkspaceRoot,
		SessionID:     r.emitSessionID,
		TurnID:        r.emitTurnID,
		CallID:        callID,
		PlanMode:      r.PlanState == PlanStatusPlanning || r.PlanState == PlanStatusProposing,
		OnStdout: func(chunk string) {
			r.emit(Event{Kind: EventToolStdout, CallID: callID, Chunk: chunk})
		},
		OnStderr: func(chunk string) {
			r.emit(Event{Kind: EventToolStderr, CallID: callID, Chunk: chunk})
		},
	}
	result, err := tool.Execute(ctx, ec, input)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

// executorFor determines which side executes a tool call. Empty string (or
// "server") means server-side execution; "client" means the client must
// execute it and deliver the result back.
func (r *Runner) executorFor(tool tools.Tool, known bool) string {
	if !known {
		return ""
	}
	if ct, ok := tool.(tools.ClientTool); ok && ct.ExecutionMode() == tools.ExecStrictClient {
		if r.ClientWaiter != nil {
			return "client"
		}
		// No client connected — fall through to server-side error handling.
	}
	return ""
}

// clientToolTimeout returns the configured client tool lease timeout, or a
// 2-minute default.
func (r *Runner) clientToolTimeout() time.Duration {
	if r.ClientToolTimeout > 0 {
		return r.ClientToolTimeout
	}
	return 2 * time.Minute
}
