package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

type Runner struct {
	Model       model.Provider
	ModelName   string
	Temperature float64
	Tools       *tools.Registry
	MaxSteps    int

	// Approver gates side-effecting tool calls. If nil, side-effecting tools are
	// denied (see approve()). Read-only tools never consult it.
	Approver Approver

	// Observer enriches each tool result into a structured Observation (P4.1).
	// Nil-safe: when unset, raw tool results are appended unchanged.
	Observer Observer

	// Reflector runs a one-shot self-check at the finalize boundary (P4.3).
	// Nil-safe: when unset, the model's first "done" is accepted as before.
	Reflector Reflector

	// RemindSkills, when true, injects a one-shot ephemeral reminder on the first
	// model call of each turn to check the Skills list and load a matching skill
	// (P6). It makes skill-loading consistent across models rather than depending
	// on a model's agency. Set by the CLI when the project has skills.
	RemindSkills bool

	Compactor session.Compactor

	// Emitter, if set, receives the turn's event stream (thinking, tool calls,
	// compaction, model latency). The loop emits; it never writes to stdout
	// itself, so the UI is fully decoupled from the runtime.
	Emitter Emitter

	// Correlation IDs stamped onto every emitted event. Set per RunTurn (which is
	// sequential on a Runner), so an event always carries which session and turn
	// produced it.
	emitSessionID string
	emitTurnID    string
}

// turnSeq backs per-turn ids; process-global and monotonic.
var turnSeq atomic.Uint64

func newTurnID() string { return fmt.Sprintf("turn_%d", turnSeq.Add(1)) }

// emit sends an event to the configured Emitter, if any. Nil-safe.
func (r *Runner) emit(e Event) {
	if r.Emitter == nil {
		return
	}
	e.At = time.Now()
	e.SessionID = r.emitSessionID
	e.TurnID = r.emitTurnID
	r.Emitter.Emit(e)
}

// TurnResult is the outcome of a single turn: the final answer the model
// produced and the tool steps taken to get there. The conversation itself lives
// on the Session, which accumulates across turns.
type TurnResult struct {
	Final        string
	Steps        []Step
	PromptTokens int

	// HitStepLimit is true when the turn exhausted MaxSteps and Final came from the
	// best-effort tool-free answer rather than the model finishing on its own. A
	// caller that delegates a turn (the subagent, 8.3) uses it to avoid passing off
	// a non-convergent run as a clean conclusion.
	HitStepLimit bool
}

const defaultMaxSteps = 24

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

	toolDefs := toolDefinitions(r.Tools)

	// Tools the model may actually call. Internal tools (registered but not
	// advertised) must not be reachable through a model call.
	advertised := make(map[string]bool, len(toolDefs))
	for _, d := range toolDefs {
		advertised[d.Function.Name] = true
	}

	r.emitSessionID = sess.ID
	r.emitTurnID = newTurnID()

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

		r.emit(Event{Kind: EventModelStarted})
		modelStart := time.Now()
		resp, err := r.Model.Complete(ctx, model.Request{
			Model:       r.ModelName,
			Temperature: r.Temperature,
			Messages:    msgs,
			Tools:       toolDefs,
		})
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

		if resp.Content != "" {
			r.emit(Event{Kind: EventThinking, Text: resp.Content})
		}

		// Execute EVERY requested tool call. Each one must get a tool result
		// with a matching tool_call_id, or the next request will be rejected.
		for _, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				return turn, err
			}
			input := json.RawMessage(call.Function.Arguments)

			step := Step{
				Index:     len(turn.Steps) + 1,
				ToolName:  call.Function.Name,
				ToolInput: input,
				StartedAt: time.Now(),
			}
			r.emit(Event{
				Kind:     EventToolStarted,
				Step:     step.Index,
				ToolName: call.Function.Name,
				ToolArgs: call.Function.Arguments,
			})

			tool, known := r.Tools.Get(call.Function.Name)

			var observation string
			var execErr error
			switch {
			case !advertised[call.Function.Name] || !known:
				execErr = fmt.Errorf("unknown tool: %s", call.Function.Name)
			case tools.HasSideEffectsFor(tool, input) && !r.approve(call.Function.Name, input):
				observation = "The user declined to run this tool. No changes were made."
			default:
				observation, execErr = r.executeTool(ctx, tool, input)
			}
			if execErr != nil {
				step.Error = execErr.Error()
				observation = "Tool error: " + execErr.Error()
			}

			// Skill telemetry (P6): if the tool loaded a skill, emit a versioned
			// event. Interface-driven, so the loop stays tool-agnostic.
			if known && execErr == nil {
				if sa, ok := tool.(tools.SkillAnnouncer); ok {
					if name, ver, loaded := sa.AnnounceSkill(input); loaded {
						r.emit(Event{Kind: EventSkillLoaded, ToolName: name, Version: ver})
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
				Step:        step.Index,
				ToolName:    call.Function.Name,
				Observation: observation,
				Err:         step.Error,
			})

			sess.Messages = append(sess.Messages, model.Message{
				Role:       model.RoleTool,
				ToolCallID: call.ID,
				Content:    observation,
			})
			sess.UpdatedAt = time.Now()
		}
	}

	// Step limit reached. Don't discard the work: the model has gathered tool
	// results in the history, so give it one final tool-free call to answer from
	// what it has — instead of a useless "stopped" message that forces the user to
	// re-ask (and re-pay for the whole investigation).
	turn.Final = r.finalAnswerAfterLimit(ctx, sess)
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

// skillsReminder is the ephemeral first-call nudge (P6). It is phrased to be
// safe across turns ("not already loaded") so a skill loaded in an earlier turn
// is not redundantly re-loaded.
const skillsReminder = "[reminder] Before you act: check the Skills list in the system prompt. " +
	"If this task matches a skill you have not already loaded, call load_skill(name) and follow " +
	"it first — that is reading project guidance, not extra investigation."

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

// finalAnswerAfterLimit makes one tool-free model call so the agent answers from
// what it already gathered when the step limit is hit. The nudge is ephemeral
// (not persisted); only the answer is appended to history, so the conversation
// continues cleanly.
func (r *Runner) finalAnswerAfterLimit(ctx context.Context, sess *session.Session) string {
	if ctx.Err() != nil {
		return stepLimitMessage
	}
	msgs := make([]model.Message, len(sess.Messages), len(sess.Messages)+1)
	copy(msgs, sess.Messages)
	msgs = append(msgs, model.Message{
		Role:    model.RoleUser,
		Content: stepLimitNudge,
	})

	r.emit(Event{Kind: EventModelStarted})
	start := time.Now()
	resp, err := r.Model.Complete(ctx, model.Request{
		Model:       r.ModelName,
		Temperature: r.Temperature,
		Messages:    msgs,
		// No Tools: the model must answer with text, not request more tools.
	})
	r.emit(Event{Kind: EventModelFinished, PromptTokens: resp.Usage.PromptTokens, Elapsed: time.Since(start), Err: errString(err)})
	if err != nil || resp.Content == "" {
		return stepLimitMessage
	}

	sess.Messages = append(sess.Messages, model.Message{Role: model.RoleAssistant, Content: resp.Content})
	sess.PromptTokens = resp.Usage.PromptTokens
	sess.UpdatedAt = time.Now()
	return resp.Content
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

func (r *Runner) executeTool(ctx context.Context, tool tools.Tool, input json.RawMessage) (string, error) {
	result, err := tool.Execute(ctx, input)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}
