package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// ExecuteNestedTool runs one workflow node through the same policy boundary as
// a model-selected tool call. It is intentionally narrower than the full
// transcript commit path: the enclosing workflow owns node state and outputs,
// while the Agent Runtime still owns approval, client dispatch, hooks, events
// and managed-tool billing receipts.
func (r *Runner) ExecuteNestedTool(
	ctx context.Context,
	parentCallID string,
	nestedCallID string,
	tool tools.Tool,
	input json.RawMessage,
) (tools.ToolResult, error) {
	if tool == nil {
		return tools.ToolResult{}, fmt.Errorf("nested tool is nil")
	}
	var domainErr error
	if active := r.Tools; active != nil {
		if (r.PlanState == PlanStatusPlanning || r.PlanState == PlanStatusProposing) && r.PlanTools != nil {
			active = r.PlanTools
		}
		allowed := false
		for _, candidate := range active.Visible() {
			if candidate.Name() == tool.Name() {
				tool, allowed = candidate, true
				break
			}
		}
		if !allowed {
			domainErr = fmt.Errorf("nested tool is unavailable in this execution domain: %s", tool.Name())
		}
	}
	if nestedCallID == "" {
		nestedCallID = parentCallID + ":" + tool.Name()
	}
	var binding tools.Binding
	if r.Tools != nil {
		binding, _ = r.Tools.Binding(tool.Name())
	}
	if binding.Name == "" {
		binding = tools.Binding{Name: tool.Name(), ModelVisibility: tools.VisibilityDirect, Executor: tools.ExecutorServer}
		if ct, ok := tool.(tools.ClientTool); ok {
			switch ct.ExecutionMode() {
			case tools.ExecStrictClient:
				binding.Executor = tools.ExecutorClient
			case tools.ExecFlex:
				binding.Executor = tools.ExecutorFlex
			}
		}
	}
	executor := r.executorFor(tool, true, binding)
	argumentErr := tools.ValidateInput(tool, input)
	eventExecutor := executor
	if argumentErr != nil || domainErr != nil {
		eventExecutor = "server"
	}
	started := time.Now()
	r.emit(Event{
		Kind:     EventToolStarted,
		CallID:   nestedCallID,
		ToolName: tool.Name(),
		ToolArgs: string(input),
		Executor: eventExecutor,
	})

	finish := func(result tools.ToolResult, err error) (tools.ToolResult, error) {
		errText := ""
		if failure := result.Err(); failure != nil {
			errText = failure.Error()
		}
		if err != nil {
			errText = err.Error()
		}
		failure := ""
		if result.Failure != nil {
			failure = result.Failure.Kind
		}
		writes := []tools.FileWrite{}
		if executor != "client" && builtinWriteTools[tool.Name()] {
			writes = append(writes, result.Wrote...)
		}
		writes = append(writes, result.NestedWrites...)
		refs := normalizeToolAssets(outputWriteAssets(result.Assets, writes, r.WorkspaceRoot, r.emitTurnID, nestedCallID), r.WorkspaceRoot, r.emitTurnID, nestedCallID)
		r.emit(Event{
			Kind:                  EventToolFinished,
			OutputEvidenceVersion: 1,
			FileWrites:            writes,
			CallID:                nestedCallID,
			ToolName:              tool.Name(),
			Observation:           result.Content,
			Output:                result.Output,
			Assets:                refs,
			ToolUsage:             result.Usage,
			Elapsed:               time.Since(started),
			Err:                   errText,
			Failure:               failure,
		})
		r.emitPreviewArtifacts(ctx, toolCallPlan{
			call:    model.ToolCall{ID: nestedCallID, Function: model.FunctionCall{Name: tool.Name()}},
			binding: binding,
		}, result.Assets, result.Previews, executor == "client")
		return result, err
	}

	if argumentErr != nil {
		return finish(tools.ToolResult{Content: argumentErr.Error(), Output: argumentErr.JSON()}, argumentErr)
	}
	if domainErr != nil {
		return finish(tools.FailureResult("unavailable", domainErr.Error()), domainErr)
	}
	if executor != "client" {
		if block := r.preHookBlock(ctx, tool.Name(), input); block != "" {
			return finish(tools.FailureResult("blocked", "The tool call was blocked. "+block), nil)
		}
		if inspector, ok := tool.(tools.Inspector); ok {
			if err := inspector.Inspect(input, r.WorkspaceRoot); err != nil {
				return finish(tools.FailureResult("blocked", "The tool call was blocked. blocked: "+err.Error()), nil)
			}
		}
	}
	if tools.HasSideEffectsFor(tool, input) && r.approve(tool.Name(), input) != VerdictAllow {
		return finish(tools.FailureResult("blocked", "The tool call was not approved. No changes were made."), nil)
	}

	if executor == "client" {
		if r.ClientWaiter == nil {
			return finish(tools.ToolResult{}, errors.New("client executor is unavailable"))
		}
		result, err := r.ClientWaiter.Wait(ctx, nestedCallID, r.clientToolTimeout())
		if err != nil {
			return finish(tools.ToolResult{}, err)
		}
		if result.IsError || result.Failure != nil {
			failure := result.Failure
			if failure == nil {
				failure = &tools.ToolFailure{Kind: "runtime", Message: result.Content}
			}
			return finish(tools.ToolResult{Content: result.Content, Output: result.Output, Assets: result.Assets, Previews: result.Previews, Failure: failure}, fmt.Errorf("%s", result.Content))
		}
		return finish(tools.ToolResult{
			Content:  result.Content,
			Output:   result.Output,
			Assets:   result.Assets,
			Previews: result.Previews,
		}, nil)
	}

	// Composite workflows own their nested history and do not yet checkpoint
	// runtime delivery receipts with the root conversation's tool history.
	result, err := r.executeTool(ctx, tool, nestedCallID, input, false)
	if err == nil && result.Err() == nil {
		r.postHook(ctx, tool.Name(), input, result.Content)
	}
	return finish(result, err)
}

var _ tools.NestedToolExecutor = (*Runner)(nil)
