package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/internal/session"
	"code-agent/internal/sessionfork"
	"code-agent/internal/tools"
)

type ForkSessionTool struct{}

func (*ForkSessionTool) Name() string { return "fork_session" }

// SideEffects marks fork_session as side-effecting: it provisions a new
// persistent session from a checkpoint (optionally an isolated worktree). The
// approval chain gates it like any other mutating tool.
func (*ForkSessionTool) SideEffects() bool { return true }
func (*ForkSessionTool) Description() string {
	return "Fork a persistent session from its latest durable, provider-valid checkpoint. The request routes through the source session's owner and preserves text/tool history in the same workspace. " +
		"Use isolated_worktree for a clean, exact-commit Git snapshot with fresh managed ownership. Gateway/local assets fail closed. The returned child is idle; use send_to_session to continue it."
}
func (*ForkSessionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Source session ID"},"name":{"type":"string"},"execution_policy":{"type":"string","enum":["shared_workspace","isolated_worktree"]},"worktree_name":{"type":"string","description":"Optional managed worktree name hint; isolated_worktree only"}},"required":["id"],"additionalProperties":false}`)
}

type forkSessionInput struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ExecutionPolicy string `json:"execution_policy"`
	WorktreeName    string `json:"worktree_name"`
}

func (*ForkSessionTool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in forkSessionInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("fork_session: parse input: %w", err)
	}
	if strings.TrimSpace(in.ID) == "" {
		return tools.ToolResult{}, fmt.Errorf("fork_session: id is required")
	}
	if in.ExecutionPolicy == "" {
		in.ExecutionPolicy = session.ExecutionPolicySharedWorkspace
	}
	if in.ExecutionPolicy != session.ExecutionPolicySharedWorkspace && in.ExecutionPolicy != session.ExecutionPolicyIsolatedWorktree {
		return tools.ToolResult{}, fmt.Errorf("fork_session: execution_policy must be shared_workspace or isolated_worktree")
	}
	if in.ExecutionPolicy == session.ExecutionPolicySharedWorkspace && strings.TrimSpace(in.WorktreeName) != "" {
		return tools.ToolResult{}, fmt.Errorf("fork_session: worktree_name requires isolated_worktree")
	}
	if ec.SessionControl == nil {
		return tools.ToolResult{}, fmt.Errorf("fork_session: control plane is not available")
	}
	requestID, err := stableToolRequestID("fork", ec)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("fork_session: request id: %w", err)
	}
	result, err := ec.SessionControl.ForkSession(ctx, sessionfork.Request{
		ParentSessionID: ec.SessionID, ParentTurnID: ec.TurnID, SourceSessionID: in.ID,
		RequestID: requestID, Name: strings.TrimSpace(in.Name), ExecutionPolicy: in.ExecutionPolicy,
		WorktreeName: strings.TrimSpace(in.WorktreeName),
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

var _ tools.SideEffecting = (*ForkSessionTool)(nil)
