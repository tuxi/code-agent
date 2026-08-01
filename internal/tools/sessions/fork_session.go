package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/internal/tools"
)

type ForkSessionTool struct{}

func (*ForkSessionTool) Name() string { return "fork_session" }
func (*ForkSessionTool) Description() string {
	return "Fork a persistent session from its latest durable, provider-valid checkpoint. The request routes through the source session's owner and preserves text/tool history in the same workspace. " +
		"Gateway/local assets and managed-worktree sources fail closed in Phase C1. The returned child is idle; use send_to_session to continue it."
}
func (*ForkSessionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Source session ID"},"name":{"type":"string"}},"required":["id"],"additionalProperties":false}`)
}

type forkSessionInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (*ForkSessionTool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in forkSessionInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("fork_session: parse input: %w", err)
	}
	if strings.TrimSpace(in.ID) == "" {
		return tools.ToolResult{}, fmt.Errorf("fork_session: id is required")
	}
	if ec.SessionControl == nil {
		return tools.ToolResult{}, fmt.Errorf("fork_session: control plane is not available")
	}
	requestID, err := stableToolRequestID("fork", ec)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("fork_session: request id: %w", err)
	}
	result, err := ec.SessionControl.ForkSession(ctx, tools.SessionForkRequest{
		ParentSessionID: ec.SessionID, ParentTurnID: ec.TurnID, SourceSessionID: in.ID,
		RequestID: requestID, Name: strings.TrimSpace(in.Name),
	})
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("fork_session: %w", err)
	}
	out, err := json.Marshal(result)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("fork_session: marshal: %w", err)
	}
	return tools.ToolResult{Content: string(out), Output: out}, nil
}
