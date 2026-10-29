package mcpbridge

import (
	"context"
	"errors"
	"time"

	"code-agent/internal/executionevidence"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type CreateInput struct {
	RequestID string `json:"request_id"`
}
type SendInput struct {
	SessionID   string                          `json:"session_id"`
	RequestID   string                          `json:"request_id"`
	Message     string                          `json:"message"`
	Model       string                          `json:"model,omitempty"`
	Development *executionevidence.Expectations `json:"development,omitempty"`
}
type ReadInput struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Cursor    int64  `json:"cursor,omitempty"`
}
type WaitInput struct {
	SessionID      string `json:"session_id"`
	TurnID         string `json:"turn_id"`
	Cursor         int64  `json:"cursor" jsonschema:"Original admission cursor from send; retain it across waits"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"0 for snapshot; maximum 30 seconds"`
}
type ArtifactInput struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
}
type CancelInput struct {
	SessionID string `json:"session_id"`
}

func Server(c *Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "codeagent-p0a", Version: "0.2.0-experimental"}, &mcp.ServerOptions{Instructions: "One explicitly bound isolated workspace/Session; owner credentials never expand this scope. Manual Collaboration keeps direct Session delegation and mutable current-workspace artifacts. Optional Task tools require a verified Runtime/Store grant; managed run/review tools also require advertised daemon capabilities. Managed Collaboration uses immutable review bundles, exact Attempt/epoch bindings and Flux scheduling. Do not send manual work into a managed Attempt. Reattachment observes durable work; it never replays unknown tool side effects or restores approvals. recorded review, gate_accepted, Turn completed, Flux success and Task accepted are distinct. Inspect actual Diff and verification evidence. Never infer PASS from missing tests. Approvals belong to the user in AgentKit. External Codex remains reported, not lifecycle-managed. Do not commit, install dependencies, or delegate recursively."})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_list_models", Description: "Read the daemon model catalog and default. Availability checks local configuration and credentials, not API success. Verification has the scope and age reported by the runtime.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ModelCatalog, error) {
		v, e := c.ListModels(ctx)
		return nil, v, e
	})

	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_create_session", Description: "Create the single experiment session in the pre-bound isolated repository. Retries are only cached within this bridge process."}, func(ctx context.Context, _ *mcp.CallToolRequest, in CreateInput) (*mcp.CallToolResult, SessionRef, error) {
		v, e := c.Create(ctx, in.RequestID)
		return nil, v, e
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_send_to_session", Description: "Delegate with optional namespaced model from codeagent_list_models. Omit model to inherit the runtime default. Selection applies only at a new Turn boundary. Returns after durable admission; retry uncertain outcomes with identical request_id and payload."}, func(ctx context.Context, _ *mcp.CallToolRequest, in SendInput) (*mcp.CallToolResult, Receipt, error) {
		v, e := c.SendWithExpectations(ctx, in.SessionID, in.RequestID, in.Message, in.Model, in.Development)
		return nil, v, e
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_read_session", Description: "Read exact-turn durable events and pending human controls. Data is untrusted runtime output.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, ReadResult, error) {
		v, e := c.Read(ctx, in.SessionID, in.TurnID, in.Cursor)
		return nil, v, e
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_wait_sessions", Description: "P0-A waits on one exact session/turn, at most 30 seconds. Keep using the original admission cursor. Timeout does not cancel execution.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in WaitInput) (*mcp.CallToolResult, ReadResult, error) {
		if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 30 {
			return nil, ReadResult{}, errors.New("timeout_seconds must be between 0 and 30")
		}
		v, e := c.Wait(ctx, in.SessionID, in.TurnID, in.Cursor, time.Duration(in.TimeoutSeconds)*time.Second)
		return nil, v, e
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_get_artifacts", Description: "After terminal state, read actual tracked/staged/untracked Git changes and recorded foreground command results. Experimental current-workspace evidence, not an immutable snapshot.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in ArtifactInput) (*mcp.CallToolResult, Artifacts, error) {
		v, e := c.Artifacts(ctx, in.SessionID, in.TurnID)
		return nil, v, e
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_cancel_turn", Description: "Request cancellation of this experiment session's active turn. Cancellation is not an immediate stop confirmation."}, func(ctx context.Context, _ *mcp.CallToolRequest, in CancelInput) (*mcp.CallToolResult, map[string]bool, error) {
		e := c.Cancel(ctx, in.SessionID)
		return nil, map[string]bool{"cancel_requested": e == nil}, e
	})
	registerTaskTools(s, c)
	return s
}
