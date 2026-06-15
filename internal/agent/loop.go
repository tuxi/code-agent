package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/prompt"
	"code-agent/internal/tools"
	"code-agent/internal/ui"
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
		r.MaxSteps = 16
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
			state.Final = decision.Message
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

			remainingSteps := r.MaxSteps - i - 1
			recentReadFiles := countRecentToolCalls(state, "read_file", 5)

			extraHint := ""
			if recentReadFiles >= 3 {
				extraHint = "\nYou have recently read several files. Prefer converging to plan, patch_proposal, or final_answer unless another file is absolutely necessary."
			}

			nextInstruction := fmt.Sprintf(
				"Tool observation for %s:\n%s\n\n"+
					"Remaining steps: %d.%s\n"+
					"Now return the next JSON decision.\n"+
					"If you have enough context for a code change, return patch_proposal.\n"+
					"If the task is too large for the remaining steps, return plan or final_answer with a concise next-step recommendation.\n"+
					"Do not keep reading files unless the missing information is essential.",
				decision.Tool,
				observation,
				remainingSteps,
				extraHint,
			)

			messages = append(messages,
				model.Message{
					Role:    model.RoleAssistant,
					Content: mustMarshalDecision(decision),
				},
				model.Message{
					Role:    model.RoleUser,
					Content: nextInstruction,
				},
			)
		case DecisionPlan:
			step.FinishedAt = time.Now()
			state.Steps = append(state.Steps, step)

			printPlan(decision)

			if decision.NeedsConfirmation {
				ok := ui.Confirm("Continue with this plan?")
				if !ok {
					state.Completed = true
					state.Final = "Plan rejected by user."
					return RunResult{
						Final: state.Final,
						State: state,
					}, nil
				}

				messages = append(messages,
					model.Message{
						Role:    model.RoleAssistant,
						Content: mustMarshalDecision(decision),
					},
					model.Message{
						Role: model.RoleUser,
						// 用户确认的消息
						Content: "The user approved the plan. Continue executing the approved plan. " +
							"Use tools only if you still need concrete file contents. " +
							"If you have enough information for the code change, return patch_proposal. " +
							"Do not repeat the same plan. " +
							"Do not modify files directly.",
					},
				)
			} else {
				messages = append(messages,
					model.Message{
						Role:    model.RoleAssistant,
						Content: mustMarshalDecision(decision),
					},
					model.Message{
						Role:    model.RoleUser,
						Content: "Continue with the next JSON decision.",
					},
				)
			}
		case DecisionPatchProposal:
			step.FinishedAt = time.Now()
			state.Steps = append(state.Steps, step)
			printPatchProposal(decision)

			if strings.TrimSpace(decision.Patch) == "" {
				state.Completed = true
				state.Final = "Patch proposal generated, but patch is empty. No files were modified."
				return RunResult{
					Final: state.Final,
					State: state,
				}, nil
			}

			ok := ui.Confirm("Validate this patch with git apply --check?")
			if !ok {
				state.Completed = true
				state.Final = "Patch proposal generated. No files were modified or validated."
				return RunResult{
					Final: state.Final,
					State: state,
				}, nil
			}

			validationInput, _ := json.Marshal(map[string]string{
				"patch": decision.Patch,
			})

			validationDecision := Decision{
				Type:   DecisionToolCall,
				Tool:   "apply_patch",
				Input:  validationInput,
				Reason: "Validate the proposed patch with git apply --check before applying anything.",
			}

			validationStartedAt := time.Now()

			validationStep := Step{
				Index:     len(state.Steps) + 1,
				Decision:  validationDecision,
				StartedAt: validationStartedAt,
			}

			validationObservation, err := r.executeTool(ctx, validationDecision)
			if err != nil {
				validationObservation = "Patch validation tool error: " + err.Error()
			}

			validationObservation = TruncateObservation(validationObservation, 8000)
			validationStep.Observation = validationObservation
			validationStep.FinishedAt = time.Now()
			state.Steps = append(state.Steps, validationStep)

			fmt.Printf("[patch validation]\n%s\n", validationObservation)

			messages = append(messages,
				model.Message{
					Role:    model.RoleAssistant,
					Content: mustMarshalDecision(decision),
				},
				model.Message{
					Role: model.RoleUser,
					Content: "Patch validation result:\n" + validationObservation + "\n\n" +
						"Now return final_answer. Summarize the proposed patch and whether it applies cleanly. " +
						"Do not claim files were modified.",
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
					Content: "Unknown decision type. Return one of: final_answer, tool_call, ask_user, plan, patch_proposal.",
				},
			)
		}
	}

	final := "Agent stopped because max steps reached."
	if len(state.Steps) > 0 {
		last := state.Steps[len(state.Steps)-1]
		final = fmt.Sprintf(
			"Agent stopped because max steps reached. Last decision=%s tool=%s error=%s",
			last.Decision.Type,
			last.Decision.Tool,
			last.Error,
		)
	}

	return RunResult{
		Final: final,
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

func printPlan(decision Decision) {
	fmt.Println("\nPlan:")
	if decision.Summary != "" {
		fmt.Println(decision.Summary)
	}

	if len(decision.Steps) > 0 {
		fmt.Println("\nSteps:")
		for i, step := range decision.Steps {
			fmt.Printf("%d. %s\n", i+1, step)
		}
	}

	if len(decision.Risks) > 0 {
		fmt.Println("\nRisks:")
		for i, risk := range decision.Risks {
			fmt.Printf("%d. %s\n", i+1, risk)
		}
	}

	fmt.Printf("\nNeeds confirmation: %v\n", decision.NeedsConfirmation)
}

func printPatchProposal(decision Decision) {
	fmt.Println("\nPatch Proposal:")

	if decision.Summary != "" {
		fmt.Println("\nSummary:")
		fmt.Println(decision.Summary)
	}

	if decision.Risk != "" {
		fmt.Println("\nRisk:")
		fmt.Println(decision.Risk)
	}

	if len(decision.Risks) > 0 {
		fmt.Println("\nRisks:")
		for i, risk := range decision.Risks {
			fmt.Printf("%d. %s\n", i+1, risk)
		}
	}

	fmt.Println("\nPatch:")
	if decision.Patch == "" {
		fmt.Println("(empty patch)")
		return
	}

	fmt.Println(decision.Patch)
}

func countRecentToolCalls(state State, toolName string, maxLookback int) int {
	count := 0
	for i := len(state.Steps) - 1; i >= 0 && len(state.Steps)-i <= maxLookback; i-- {
		step := state.Steps[i]
		if step.Decision.Type == DecisionToolCall && step.Decision.Tool == toolName {
			count++
		}
	}
	return count
}
