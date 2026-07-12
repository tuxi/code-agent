package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

// maxParallelTools is the effective concurrency bound for one tool-call batch.
// <= 1 means strictly sequential (the pre-P8.8 behavior).
func (r *Runner) maxParallelTools() int {
	if r.MaxParallelTools < 1 {
		return 1
	}
	return r.MaxParallelTools
}

// toolCallPlan is the main-goroutine-resolved description of one tool call:
// everything needed to execute it, computed deterministically in model order
// (tool lookup, step index, web_search budget ordinal) so the concurrent
// execution phase stays pure.
type toolCallPlan struct {
	call             model.ToolCall
	input            json.RawMessage
	step             Step
	tool             tools.Tool
	known            bool
	valid            bool
	executor         string
	webSearchOrdinal int // 1-based web_search ordinal this turn; 0 if not a web_search
}

// parallelizable reports whether this call may run concurrently with its
// neighbors. Only valid, server-executed, read-only calls qualify: unknown
// tools, client-executed tools, and any side-effecting call (which may prompt
// for approval or mutate the workspace) are serial barriers, so no two effects
// ever overlap and at most one approval is pending at a time (P8.8 §4.1/§6).
func (p toolCallPlan) parallelizable() bool {
	if !p.valid || p.executor == "client" {
		return false
	}
	return !tools.HasSideEffectsFor(p.tool, p.input)
}

// toolCallResult is a worker's pure output: the observation and side-channels,
// with no shared-state mutation. The main goroutine commits it in order.
type toolCallResult struct {
	observation string
	output      json.RawMessage
	assetRefs   []assets.Ref
	stepError   string // non-empty => a genuine tool error (not a cancel)
	toolStart   time.Time
}

// executeToolBatch runs one assistant message's tool calls under the
// read-parallel/write-serial policy (P8.8), appends a tool-result message and a
// Step per call in model order, and emits per-call events. It returns a non-nil
// error only when the batch was cut short by context cancellation; in that case
// every not-yet-run call still gets a synthetic interrupted result, so history
// stays balanced (the tool_calls message references every call, and a missing
// result makes the provider reject the resume with "insufficient tool messages
// following tool_calls"). On resume the model sees they did not run and
// re-issues what it still needs (v1.2 §2.2).
func (r *Runner) executeToolBatch(
	ctx context.Context,
	sess *session.Session,
	turn *TurnResult,
	calls []model.ToolCall,
	activeTools *tools.Registry,
	advertised map[string]bool,
	webSearches *int,
	turnAssets *[]assets.Ref,
) error {
	// Phase 1 — plan every call in model order (deterministic, main goroutine):
	// resolve the tool, assign the step index, and count the web_search budget.
	base := len(turn.Steps)
	plans := make([]toolCallPlan, len(calls))
	for i, call := range calls {
		input := json.RawMessage(call.Function.Arguments)
		tool, known := activeTools.Get(call.Function.Name)
		p := toolCallPlan{
			call:     call,
			input:    input,
			tool:     tool,
			known:    known,
			valid:    advertised[call.Function.Name] && known,
			executor: r.executorFor(tool, known),
			step: Step{
				Index:     base + i + 1,
				ToolName:  call.Function.Name,
				ToolInput: input,
				StartedAt: time.Now(),
			},
		}
		if call.Function.Name == webSearchToolName {
			*webSearches++
			p.webSearchOrdinal = *webSearches
		}
		plans[i] = p
	}

	// Phase 2 — partition into groups (order-preserving): a maximal run of
	// consecutive parallelizable calls, capped at the concurrency bound, or a
	// single barrier call.
	groups := partitionToolGroups(plans, r.maxParallelTools())

	// Phase 3 — for each group in order: cancel-check, emit ToolStarted, execute
	// (concurrently when the group has >1 call), then commit results in order.
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			for _, p := range plans[g[0]:] {
				sess.Messages = append(sess.Messages, model.Message{
					Role:       model.RoleTool,
					ToolCallID: p.call.ID,
					Content:    toolInterruptedObservation,
				})
			}
			sess.UpdatedAt = time.Now()
			return err
		}

		group := plans[g[0]:g[1]]
		for _, p := range group {
			r.emit(Event{
				Kind:     EventToolStarted,
				CallID:   p.call.ID,
				Step:     p.step.Index,
				ToolName: p.call.Function.Name,
				ToolArgs: p.call.Function.Arguments,
				Executor: p.executor,
			})
		}

		results := make([]toolCallResult, len(group))
		if len(group) == 1 {
			results[0] = r.runToolCall(ctx, group[0])
		} else {
			var wg sync.WaitGroup
			for k := range group {
				wg.Add(1)
				go func(k int) {
					defer wg.Done()
					results[k] = r.runToolCall(ctx, group[k]) // distinct index: no race
				}(k)
			}
			wg.Wait()
		}

		for k := range group {
			r.commitToolResult(ctx, sess, turn, turnAssets, group[k], results[k])
		}
	}
	return nil
}

// partitionToolGroups splits plans into contiguous groups. A run of consecutive
// parallelizable calls becomes one group (capped at maxParallel so a long run
// executes in bounded waves); every non-parallelizable call is its own group.
// maxParallel <= 1 makes every call its own group — i.e. fully sequential,
// byte-identical to the pre-P8.8 loop.
func partitionToolGroups(plans []toolCallPlan, maxParallel int) [][2]int {
	var groups [][2]int
	for i := 0; i < len(plans); {
		if maxParallel <= 1 || !plans[i].parallelizable() {
			groups = append(groups, [2]int{i, i + 1})
			i++
			continue
		}
		j := i
		for j < len(plans) && plans[j].parallelizable() && j-i < maxParallel {
			j++
		}
		groups = append(groups, [2]int{i, j})
		i = j
	}
	return groups
}

// runToolCall executes one planned call and returns its result. It is pure with
// respect to loop state (it mutates nothing shared) so it is safe to run in a
// goroutine; it may still emit stdout/stderr chunks through r.emit, which is
// serialized. This is the concurrent phase.
func (r *Runner) runToolCall(ctx context.Context, p toolCallPlan) toolCallResult {
	res := toolCallResult{toolStart: time.Now()}

	// Pre-tool hook (8.5): a configured command may block the call. Only
	// consulted for a real tool, so an unknown call still reports plainly.
	var blockReason string
	if p.valid && p.executor != "client" {
		blockReason = r.preHookBlock(ctx, p.call.Function.Name, p.input)
	}

	// Inspect (P0): tool-specific static safety validation. Runs after the
	// pre-tool hook and before any policy/permission decision — pure static
	// analysis, no I/O, no human prompt. Mirrors Claude Code's
	// tool.validateInput() stage. Tools that don't implement Inspector skip
	// this with zero overhead.
	if p.valid && p.executor != "client" && blockReason == "" {
		if inspector, ok := p.tool.(tools.Inspector); ok {
			if err := inspector.Inspect(p.input, r.WorkspaceRoot); err != nil {
				blockReason = "blocked: " + err.Error()
			}
		}
	}

	var observation string
	var output json.RawMessage
	var assetRefs []assets.Ref
	var execErr error
	switch {
	case !p.valid:
		execErr = fmt.Errorf("unknown tool: %s", p.call.Function.Name)
	case p.webSearchOrdinal > 0 && p.webSearchOrdinal > r.maxWebSearches():
		observation = fmt.Sprintf(
			"Search budget reached: %d web searches already this turn (limit %d). "+
				"Stop searching — reformulating the query will not surface new results. "+
				"Answer with the results you already have, or web_fetch a specific URL.",
			p.webSearchOrdinal-1, r.maxWebSearches())
	case blockReason != "":
		observation = "The tool call was blocked. " + blockReason
	case tools.HasSideEffectsFor(p.tool, p.input) && r.approve(p.call.Function.Name, p.input) != VerdictAllow:
		observation = "The tool call was not approved. No changes were made."
	case p.executor == "client":
		result, waitErr := r.ClientWaiter.Wait(ctx, p.call.ID, r.clientToolTimeout())
		if waitErr != nil {
			execErr = waitErr
		} else if result.IsError {
			execErr = fmt.Errorf("%s", result.Content)
		} else {
			observation = result.Content
			output = result.Output
			assetRefs = result.Assets
		}
	default:
		var toolResult tools.ToolResult
		toolResult, execErr = r.executeTool(ctx, p.tool, p.call.ID, p.input)
		if execErr == nil {
			observation = toolResult.Content
			output = toolResult.Output
			assetRefs = toolResult.Assets
			// Post-tool hook (8.5): react to the change (format/lint). It runs the
			// configured command but does not alter the result in v1.
			r.postHook(ctx, p.call.Function.Name, p.input, observation)
		}
	}
	if execErr != nil {
		if errors.Is(execErr, context.Canceled) {
			// Interrupted by a suspend/cancel, not a genuine failure. Record the
			// neutral marker (no error) so history stays resumable and the client
			// sees no spurious "context canceled".
			observation = toolInterruptedObservation
		} else {
			res.stepError = execErr.Error()
			observation = "Tool error: " + execErr.Error()
		}
	}
	res.observation = observation
	res.output = output
	res.assetRefs = assetRefs
	return res
}

// commitToolResult finalizes one call on the main goroutine, in model order:
// skill/todo telemetry, Observation enrichment, the Step, asset normalization,
// the ToolFinished event, and the tool-result message.
func (r *Runner) commitToolResult(ctx context.Context, sess *session.Session, turn *TurnResult, turnAssets *[]assets.Ref, p toolCallPlan, res toolCallResult) {
	observation := res.observation
	step := p.step
	step.Error = res.stepError

	// Skill/Todo telemetry (P6/8.4): interface-driven, only for a known tool that
	// executed cleanly. res.stepError == "" mirrors the old execErr == nil gate
	// (an interrupted call leaves stepError empty and announces nothing loaded).
	if p.known && res.stepError == "" {
		if sa, ok := p.tool.(tools.SkillAnnouncer); ok {
			if name, ver, src, loaded := sa.AnnounceSkill(p.input); loaded {
				r.emit(Event{Kind: EventSkillLoaded, ToolName: name, Version: ver, SkillSource: src})
			}
		}
		if ta, ok := p.tool.(tools.TodoAnnouncer); ok {
			if todos, ok := ta.AnnounceTodos(p.input); ok {
				r.emit(Event{Kind: EventTodoUpdated, Todos: todos})
			}
		}
	}

	// Enrich into a structured Observation (P4.1): Observe on the full output so
	// salient lines survive truncation, then truncate with the summary prepended.
	if r.Observer != nil {
		obs := r.Observer.Observe(p.call.Function.Name, observation)
		observation = obs.Render(TruncateObservation(observation, maxObservationBytes))
		r.emit(Event{
			Kind:        EventObserved,
			CallID:      p.call.ID,
			Step:        step.Index,
			ToolName:    p.call.Function.Name,
			Observation: obs.Summary,
			Failure:     string(obs.FailureType),
		})
	} else {
		observation = TruncateObservation(observation, maxObservationBytes)
	}

	step.Observation = observation
	step.FinishedAt = time.Now()
	turn.Steps = append(turn.Steps, step)
	assetRefs := normalizeToolAssets(res.assetRefs, r.WorkspaceRoot, r.emitTurnID, p.call.ID)
	gatewayAssets, assetNote := r.gatewayScreenshotAssets(ctx, sess, p.call.Function.Name, assetRefs)
	if assetNote != "" {
		observation += "\n" + assetNote
		step.Observation = observation
		turn.Steps[len(turn.Steps)-1].Observation = observation
	}
	*turnAssets = append(*turnAssets, assetRefs...)

	r.emit(Event{
		Kind:        EventToolFinished,
		CallID:      p.call.ID,
		Step:        step.Index,
		ToolName:    p.call.Function.Name,
		Observation: observation,
		Output:      res.output,
		Assets:      assetRefs,
		Elapsed:     time.Since(res.toolStart),
		Err:         step.Error,
	})

	sess.Messages = append(sess.Messages, model.Message{
		Role:       model.RoleTool,
		ToolCallID: p.call.ID,
		Content:    observation,
		Assets:     gatewayAssets,
	})
	sess.UpdatedAt = time.Now()
}
