package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
	if nestedCallID == "" {
		nestedCallID = parentCallID + ":" + tool.Name()
	}
	executor := r.executorFor(tool, true)
	started := time.Now()
	r.emit(Event{
		Kind:     EventToolStarted,
		CallID:   nestedCallID,
		ToolName: tool.Name(),
		ToolArgs: string(input),
		Executor: executor,
	})

	finish := func(result tools.ToolResult, err error) (tools.ToolResult, error) {
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		r.emit(Event{
			Kind:        EventToolFinished,
			CallID:      nestedCallID,
			ToolName:    tool.Name(),
			Observation: result.Content,
			Output:      result.Output,
			Assets:      result.Assets,
			ToolUsage:   result.Usage,
			Elapsed:     time.Since(started),
			Err:         errText,
		})
		return result, err
	}

	if executor != "client" {
		if block := r.preHookBlock(ctx, tool.Name(), input); block != "" {
			return finish(tools.ToolResult{Content: "The tool call was blocked. " + block}, nil)
		}
		if inspector, ok := tool.(tools.Inspector); ok {
			if err := inspector.Inspect(input, r.WorkspaceRoot); err != nil {
				return finish(tools.ToolResult{Content: "The tool call was blocked. blocked: " + err.Error()}, nil)
			}
		}
	}
	if tools.HasSideEffectsFor(tool, input) && r.approve(tool.Name(), input) != VerdictAllow {
		return finish(tools.ToolResult{Content: "The tool call was not approved. No changes were made."}, nil)
	}

	if executor == "client" {
		result, err := r.ClientWaiter.Wait(ctx, nestedCallID, r.clientToolTimeout())
		if err != nil {
			return finish(tools.ToolResult{}, err)
		}
		if result.IsError {
			return finish(tools.ToolResult{}, fmt.Errorf("%s", result.Content))
		}
		return finish(tools.ToolResult{
			Content: result.Content,
			Output:  result.Output,
			Assets:  result.Assets,
		}, nil)
	}

	result, err := r.executeTool(ctx, tool, nestedCallID, input)
	if err == nil {
		r.postHook(ctx, tool.Name(), input, result.Content)
	}
	return finish(result, err)
}

var _ tools.NestedToolExecutor = (*Runner)(nil)
