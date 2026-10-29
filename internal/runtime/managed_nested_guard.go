package runtime

import (
	"context"
	"encoding/json"

	"code-agent/internal/collab"
	"code-agent/internal/tools"
)

// A legacy workflow can outlive its initiating Turn. Lease each actual tool
// invocation as well, so a later managed claim cannot race that writer and a
// queued/retried generic workflow cannot enter a claimed workspace.
type leasedNestedExecutor struct {
	next            tools.NestedToolExecutor
	guard           collab.ExecutionGuard
	root, sessionID string
}

func (e *leasedNestedExecutor) ExecuteNestedTool(ctx context.Context, parent, callID string, t tools.Tool, input json.RawMessage) (tools.ToolResult, error) {
	release, err := e.guard.AcquireTaskExecution(ctx, e.sessionID, e.root, "workflow:"+callID, nil)
	if err != nil {
		return tools.ToolResult{}, err
	}
	defer release()
	return e.next.ExecuteNestedTool(ctx, parent, callID, t, input)
}
