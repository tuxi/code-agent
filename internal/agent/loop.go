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

	for i := 0; i < r.MaxSteps; i++ {
		// Compact before each model call, not just at the turn boundary: a single
		// turn with many tool calls can grow the prompt past the threshold mid-loop.
		if err := r.maybeCompact(ctx, sess); err != nil {
			return turn, err
		}

		r.emit(Event{Kind: EventModelStarted})
		modelStart := time.Now()
		resp, err := r.Model.Complete(ctx, model.Request{
			Model:       r.ModelName,
			Temperature: r.Temperature,
			Messages:    sess.Messages,
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

		// The assistant turn must be kept in history verbatim: the API requires
		// the tool_calls message to precede the tool results that answer it.
		sess.Messages = append(sess.Messages, resp.AssistantMessage())
		sess.UpdatedAt = time.Now()

		// No tool calls => the model is done; this is the final answer.
		if !resp.HasToolCalls() {
			turn.Final = resp.Content
			r.emit(Event{Kind: EventTurnFinished, Text: turn.Final})
			return turn, nil
		}

		if resp.Content != "" {
			r.emit(Event{Kind: EventThinking, Text: resp.Content})
		}

		// Execute EVERY requested tool call. Each one must get a tool result
		// with a matching tool_call_id, or the next request will be rejected.
		for _, call := range resp.ToolCalls {
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
			case tools.HasSideEffects(tool) && !r.approve(call.Function.Name, input):
				observation = "The user declined to run this tool. No changes were made."
			default:
				observation, execErr = r.executeTool(ctx, tool, input)
			}
			if execErr != nil {
				step.Error = execErr.Error()
				observation = "Tool error: " + execErr.Error()
			}
			observation = TruncateObservation(observation, 8000)

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

	turn.Final = "Agent stopped: reached the step limit before finishing."
	r.emit(Event{Kind: EventTurnFinished, Text: turn.Final})
	return turn, nil
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
