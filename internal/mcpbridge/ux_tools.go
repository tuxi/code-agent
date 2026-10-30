package mcpbridge

import (
	"context"
	"time"

	"code-agent/internal/collab"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type BindingInfo struct {
	Workspace        string `json:"workspace_path"`
	Baseline         string `json:"baseline_commit"`
	SessionID        string `json:"session_id,omitempty"`
	RuntimeID        string `json:"server_id,omitempty"`
	StoreID          string `json:"store_id,omitempty"`
	IdentityVerified bool   `json:"identity_verified"`
}

type HandoffInput struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	TaskID    string `json:"task_id,omitempty" jsonschema:"Optional existing Task for verified exact Attempt and authoritative Task state; does not create associations"`
}

func (c *Client) BindingInfo(ctx context.Context) (BindingInfo, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.checkWorkspaceIdentity(); err != nil {
		return BindingInfo{}, err
	}
	if err := c.checkBinding(); err != nil {
		return BindingInfo{}, err
	}
	b := BindingInfo{Workspace: c.root, Baseline: c.baseline, SessionID: c.id}
	if c.binding != nil {
		if c.binding.verifiedStoreID != "" {
			if err := c.verifyRuntimeIdentity(ctx); err != nil {
				return BindingInfo{}, err
			}
		} else {
			var info struct {
				ServerID string `json:"server_id"`
			}
			if err := c.request(ctx, "GET", "/v1/runtime/info", nil, &info); err != nil {
				return BindingInfo{}, err
			}
			if info.ServerID != c.binding.verifiedServerID {
				return BindingInfo{}, collab.ErrForbidden
			}
		}
		b.RuntimeID, b.StoreID, b.IdentityVerified = c.binding.verifiedServerID, c.binding.verifiedStoreID, true
	}
	return b, nil
}

func registerCollaborationUXTools(s *mcp.Server, c *Client) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_connection_info", Description: "Check the explicitly bound workspace, baseline, Session and granted Runtime/Store identity. Never retarget a mismatched workspace. If this native tool is unavailable, diagnose project config instead of constructing a PTY/stdio driver.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, BindingInfo, error) {
		v, e := c.BindingInfo(ctx)
		return nil, v, e
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_lookup_admission", Description: "Read durable admission by stable request_id after a missing send receipt. unknown does NOT mean unsubmitted. Never allocate a new request_id to bypass uncertainty; observe an admitted Turn or retry only identical request_id/payload in the original process.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, in AdmissionInput) (*mcp.CallToolResult, Admission, error) {
		v, e := c.LookupAdmission(ctx, in.SessionID, in.RequestID)
		return nil, v, e
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_wait_for_update", Description: "Optional only when user explicitly requests waiting; long tasks default to submit and end response. For requested quiet wait use timeout_seconds=30 and advance after_cursor from each response. Returns lifecycle/tool-result milestones and exact human attention references, never transcript/logs. Timeout does not cancel. Terminal points to get_handoff; detailed evidence is opt-in.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, in UpdateInput) (*mcp.CallToolResult, Update, error) {
		v, e := c.WaitForUpdate(ctx, in.SessionID, in.TurnID, in.AfterCursor, time.Duration(in.TimeoutSeconds)*time.Second)
		return nil, v, e
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_get_handoff", Description: "After terminal: compact recorded command/exit-code/evidence index, changed filenames and current Git diff hash. No transcript, diff body or logs. Declare development.validation_commands AND instruct Executor to run them when sending. Missing checks stay partial/unknown. Manual evidence is mutable; acceptance remains not_reviewed. Optional task_id resolves an existing exact Attempt. Use get_artifacts/read_session only for needed detail.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, in HandoffInput) (*mcp.CallToolResult, Handoff, error) {
		v, e := c.HandoffForTask(ctx, in.SessionID, in.TurnID, in.TaskID)
		return nil, v, e
	})
}
