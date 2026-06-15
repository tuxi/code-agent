package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/prompt"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Runner struct {
	Model       model.Provider
	ModelName   string
	Temperature float64
	Tools       *tools.Registry
	MaxSteps    int
}

type RunResult struct {
	Final string
	State State
}

func (r *Runner) Run(ctx context.Context, goal string) (RunResult, error) {
	if r.Model == nil {
		return RunResult{}, errors.New("missing model provider")
	}
	if r.Tools == nil {
		r.Tools = tools.NewRegistry()
	}
	if r.MaxSteps <= 0 {
		r.MaxSteps = 8
	}

	state := State{
		Goal:     goal,
		MaxSteps: r.MaxSteps,
	}

	messages := []model.Message{
		{
			Role:    model.RoleSystem,
			Content: prompt.AgentSystemPrompt,
		},
		{
			Role: model.RoleUser,
			Content: "Goal:\n" + goal + "\n\n" +
				"Start by deciding whether you need to call a tool. Return exactly one JSON decision.",
		},
	}

	for i := 0; i < r.MaxSteps; i++ {
		startedAt := time.Now()
		resp, err := r.Model.Complete(ctx, model.Request{
			Messages:    messages,
			Model:       r.ModelName,
			Temperature: r.Temperature,
		})
		if err != nil {
			return RunResult{State: state}, err
		}

		decision, err := ParseDecision(resp.Content)
		if err != nil {
			messages = append(messages,
				model.Message{
					Role:    model.RoleAssistant,
					Content: resp.Content,
				}, model.Message{
					Role: model.RoleUser,
					Content: "Your previous response was not valid JSON Decision. Error: " + err.Error() +
						"\nReturn exactly one JSON object.",
				},
			)
			continue
		}

		step := Step{
			Index:      i + 1,
			Decision:   decision,
			StartedAt:  startedAt,
			FinishedAt: time.Now(),
		}
		fmt.Printf("\n[%d] decision=%s", step.Index, decision.Type)
		if decision.Tool != "" {
			fmt.Printf(" tool=%s", decision.Tool)
		}
		if decision.Reason != "" {
			fmt.Printf(" reason=%s", decision.Reason)
		}
		fmt.Println()

		switch decision.Type {
		case DecisionFinalAnswer:
			state.Completed = true
			state.Final = decision.Reason
			state.Steps = append(state.Steps, step)
			return RunResult{
				Final: decision.Message,
				State: state,
			}, nil
		case DecisionAskUser:
			state.Completed = true
			state.Final = "Agent asks user: " + decision.Message
			state.Steps = append(state.Steps, step)
			return RunResult{
				Final: state.Final,
				State: state,
			}, nil
		case DecisionToolCall:
			observation, err := r.executeTool(ctx, decision)
			if err != nil {
				step.Error = err.Error()
				observation = "Tool error: " + err.Error()
			}
			// read_file 读出来如果文件比较大，虽然有 MaxBytes，但返回给模型的 observation 还是可能很长
			// 默认max最多 8000 或 12000 字符
			// 超过则追加 <truncated>
			observation = TruncateObservation(observation, 8000)

			step.Observation = observation
			step.FinishedAt = time.Now()
			state.Steps = append(state.Steps, step)

			fmt.Printf("[observation]\n%s\n", observation)

			messages = append(messages,
				model.Message{
					Role:    model.RoleAssistant,
					Content: mustMarshalDecision(decision),
				},
				model.Message{
					Role: model.RoleUser,
					Content: "Tool observation for " + decision.Tool + ":\n" + observation + "\n\n" +
						"Now return the next JSON decision.",
				},
			)
		default:
			step.Error = "unknown decision type: " + string(decision.Type)

			state.Steps = append(state.Steps, step)
			messages = append(messages,
				model.Message{
					Role:    model.RoleAssistant,
					Content: resp.Content,
				},
				model.Message{
					Role:    model.RoleUser,
					Content: "Unknown decision type. Return one of: final_answer, tool_call, ask_user.",
				},
			)
		}
	}
	return RunResult{
		Final: "Agent stopped because max steps reached.",
		State: state,
	}, nil
}

func (r *Runner) executeTool(ctx context.Context, decision Decision) (string, error) {

	if decision.Tool == "" {
		return "", fmt.Errorf("missing tool name")
	}
	tool, ok := r.Tools.Get(decision.Tool)
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", decision.Tool)
	}
	result, err := tool.Execute(ctx, decision.Input)
	if err != nil {
		return "", err
	}
	return result.Content, nil

}

func ParseDecision(content string) (Decision, error) {

	cleaned := strings.TrimSpace(content)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)
	var decision Decision
	if err := json.Unmarshal([]byte(cleaned), &decision); err != nil {
		return Decision{}, err
	}
	if decision.Type == "" {
		return Decision{}, fmt.Errorf("missing decision type")
	}
	return decision, nil

}

func mustMarshalDecision(decision Decision) string {

	data, err := json.Marshal(decision)
	if err != nil {
		return `{"type":"final_answer","message":"failed to marshal previous decision"}`
	}
	return string(data)

}
