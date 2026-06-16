package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/prompt"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type RunResult struct {
	Final string
	State State
}

const defaultMaxSteps = 24

// Run drives the agent as a single uniform loop:
//
//	call the model (with the tool schemas) -> if it returns no tool calls, that
//	is the final answer; otherwise execute every requested tool call, feed each
//	result back as a tool message, and loop.
//
// The loop contains no per-tool logic and no decision-type state machine. The
// model owns the control flow; the runtime executes tools, gates the ones with
// side effects, and records what happened.
func (r *Runner) Run(ctx context.Context, goal string) (RunResult, error) {
	if r.Model == nil {
		return RunResult{}, errors.New("missing model provider")
	}
	if r.Tools == nil {
		r.Tools = tools.NewRegistry()
	}
	if r.MaxSteps <= 0 {
		r.MaxSteps = defaultMaxSteps
	}

	state := State{Goal: goal, MaxSteps: r.MaxSteps}

	toolDefs := toolDefinitions(r.Tools)

	// The set of tools the model is actually allowed to call. Internal tools
	// (registered but not advertised) must not be reachable via a model call.
	advertised := make(map[string]bool, len(toolDefs))
	for _, d := range toolDefs {
		advertised[d.Function.Name] = true
	}

	messages := []model.Message{
		{Role: model.RoleSystem, Content: prompt.AgentSystemPrompt},
		{Role: model.RoleUser, Content: goal},
	}

	for i := 0; i < r.MaxSteps; i++ {
		resp, err := r.Model.Complete(ctx, model.Request{
			Model:       r.ModelName,
			Temperature: r.Temperature,
			Messages:    messages,
			Tools:       toolDefs,
		})
		if err != nil {
			return RunResult{State: state}, err
		}

		// The assistant turn must be kept in history verbatim: the API requires
		// the tool_calls message to precede the tool results that answer it.
		messages = append(messages, resp.AssistantMessage())

		// No tool calls => the model is done. This is the final answer.
		if !resp.HasToolCalls() {
			state.Completed = true
			state.Final = resp.Content
			return RunResult{Final: resp.Content, State: state}, nil
		}

		if resp.Content != "" {
			fmt.Printf("\n[thinking] %s\n", resp.Content)
		}

		// Execute EVERY requested tool call. Each one must get a tool result
		// with a matching tool_call_id, or the next request will be rejected.
		for _, call := range resp.ToolCalls {
			input := json.RawMessage(call.Function.Arguments)

			step := Step{
				Index:     len(state.Steps) + 1,
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
			state.Steps = append(state.Steps, step)

			fmt.Printf("[observation]\n%s\n", observation)

			messages = append(messages, model.Message{
				Role:       model.RoleTool,
				ToolCallID: call.ID,
				Content:    observation,
			})
		}
	}

	final := "Agent stopped: reached the step limit before finishing."
	state.Final = final
	return RunResult{Final: final, State: state}, nil
}

func (r *Runner) executeTool(ctx context.Context, tool tools.Tool, input json.RawMessage) (string, error) {
	result, err := tool.Execute(ctx, input)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}
