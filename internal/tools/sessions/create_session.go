package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/internal/tools"
)

type CreateSessionTool struct{}

func (*CreateSessionTool) Name() string { return "create_session" }
func (*CreateSessionTool) Description() string {
	return "Create a new persistent child session owned by this Runtime. The child starts empty in the requested workspace; use send_to_session to give it work. " +
		"The parent-child spawn edge is recorded durably. Use isolated_worktree to provision an owned Git worktree from workspace_path."
}
func (*CreateSessionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"workspace_path":{"type":"string","description":"Absolute target workspace, or source Git workspace for isolated_worktree; defaults to the caller workspace"},"name":{"type":"string"},"execution_policy":{"type":"string","enum":["shared_workspace","read_only","isolated_worktree"]},"worktree_name":{"type":"string","description":"Optional branch/worktree name hint; isolated_worktree only"},"base_ref":{"type":"string","enum":["head","fresh"],"description":"Git base for isolated_worktree; defaults to head"}},"additionalProperties":false}`)
}

type createSessionInput struct {
	WorkspacePath   string `json:"workspace_path"`
	Name            string `json:"name"`
	ExecutionPolicy string `json:"execution_policy"`
	WorktreeName    string `json:"worktree_name"`
	BaseRef         string `json:"base_ref"`
}

func (*CreateSessionTool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in createSessionInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("create_session: parse input: %w", err)
	}
	if ec.SessionControl == nil {
		return tools.ToolResult{}, fmt.Errorf("create_session: control plane is not available")
	}
	if in.WorkspacePath == "" {
		in.WorkspacePath = ec.WorkspaceRoot
	}
	if !strings.HasPrefix(in.WorkspacePath, "/") {
		return tools.ToolResult{}, fmt.Errorf("create_session: workspace_path must be absolute")
	}
	if in.ExecutionPolicy == "" {
		in.ExecutionPolicy = "shared_workspace"
	}
	if in.ExecutionPolicy != "shared_workspace" && in.ExecutionPolicy != "read_only" && in.ExecutionPolicy != "isolated_worktree" {
		return tools.ToolResult{}, fmt.Errorf("create_session: execution_policy must be shared_workspace, read_only, or isolated_worktree")
	}
	if in.ExecutionPolicy == "isolated_worktree" {
		if in.BaseRef == "" {
			in.BaseRef = "head"
		}
		if in.BaseRef != "head" && in.BaseRef != "fresh" {
			return tools.ToolResult{}, fmt.Errorf("create_session: base_ref must be head or fresh")
		}
	} else if in.WorktreeName != "" || in.BaseRef != "" {
		return tools.ToolResult{}, fmt.Errorf("create_session: worktree_name and base_ref require isolated_worktree")
	}
	requestID, err := stableToolRequestID("spawn", ec)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("create_session: request id: %w", err)
	}
	result, err := ec.SessionControl.CreateSession(ctx, tools.SessionCreateRequest{
		ParentSessionID: ec.SessionID, ParentTurnID: ec.TurnID, RequestID: requestID,
		WorkspacePath: in.WorkspacePath, Name: strings.TrimSpace(in.Name), ExecutionPolicy: in.ExecutionPolicy,
		WorktreeName: strings.TrimSpace(in.WorktreeName), BaseRef: in.BaseRef,
	})
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("create_session: %w", err)
	}
	out, err := json.Marshal(result)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("create_session: marshal: %w", err)
	}
	return tools.ToolResult{Content: string(out), Output: out}, nil
}
