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
}

// TurnResult is the outcome of a single turn: the final answer the model
// produced and the tool steps taken to get there. The conversation itself lives
// on the Session, which accumulates across turns.
type TurnResult struct {
	Final string
	Steps []Step
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

	// Append the user's turn to the persistent session history.
	sess.Messages = append(sess.Messages, model.Message{
		Role:    model.RoleUser,
		Content: userInput,
	})
	sess.UpdatedAt = time.Now()

	var turn TurnResult

	for i := 0; i < r.MaxSteps; i++ {
		resp, err := r.Model.Complete(ctx, model.Request{
			Model:       r.ModelName,
			Temperature: r.Temperature,
			Messages:    sess.Messages,
			Tools:       toolDefs,
		})
		if err != nil {
			return turn, err
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
			return turn, nil
		}

		if resp.Content != "" {
			fmt.Printf("\n[thinking] %s\n", resp.Content)
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
			fmt.Printf("\n[%d] tool=%s args=%s\n", step.Index, call.Function.Name, call.Function.Arguments)

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

			fmt.Printf("[observation]\n%s\n", observation)

			sess.Messages = append(sess.Messages, model.Message{
				Role:       model.RoleTool,
				ToolCallID: call.ID,
				Content:    observation,
			})
			sess.UpdatedAt = time.Now()
		}
	}

	turn.Final = "Agent stopped: reached the step limit before finishing."
	return turn, nil
}

func (r *Runner) executeTool(ctx context.Context, tool tools.Tool, input json.RawMessage) (string, error) {
	result, err := tool.Execute(ctx, input)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}
