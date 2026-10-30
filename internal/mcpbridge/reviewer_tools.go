package mcpbridge

import (
	"context"
	"errors"
	"time"

	"code-agent/internal/collab"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errTimeoutRange = errors.New("timeout_seconds must be between 0 and 30")

// ReviewReader is the read-only surface a reviewer profile is allowed to use.
// Both a live observation binding and an offline evidence bundle satisfy it;
// neither can create, send, cancel, or run anything.
type ReviewReader interface {
	BindingInfo(ctx context.Context) (BindingInfo, error)
	ReadSession(ctx context.Context, turnID string, cursor int64) (ReadResult, error)
	AwaitUpdate(ctx context.Context, turnID string, cursor int64, timeout time.Duration) (Update, error)
	ReviewHandoff(ctx context.Context, turnID, taskID string) (Handoff, error)
	ReviewArtifacts(ctx context.Context, req ArtifactRequest) (Artifacts, error)
	TaskSnapshot(ctx context.Context, taskID string) (collab.Snapshot, error)
	ReviewBundle(ctx context.Context, taskID, artifactID string) (collab.ReviewBundle, error)
}

// clientReviewReader adapts a live Client. The Client is already bound to one
// Session, so the reviewer tool inputs cannot retarget another one.
type clientReviewReader struct{ c *Client }

func (r clientReviewReader) BindingInfo(ctx context.Context) (BindingInfo, error) {
	return r.c.BindingInfo(ctx)
}
func (r clientReviewReader) ReadSession(ctx context.Context, turnID string, cursor int64) (ReadResult, error) {
	return r.c.Read(ctx, r.c.BoundSessionID(), turnID, cursor)
}
func (r clientReviewReader) AwaitUpdate(ctx context.Context, turnID string, cursor int64, timeout time.Duration) (Update, error) {
	return r.c.WaitForUpdate(ctx, r.c.BoundSessionID(), turnID, cursor, timeout)
}
func (r clientReviewReader) ReviewHandoff(ctx context.Context, turnID, taskID string) (Handoff, error) {
	return r.c.HandoffForTask(ctx, r.c.BoundSessionID(), turnID, taskID)
}
func (r clientReviewReader) ReviewArtifacts(ctx context.Context, req ArtifactRequest) (Artifacts, error) {
	a, err := r.c.Artifacts(ctx, r.c.BoundSessionID(), req.TurnID)
	if err != nil {
		return Artifacts{}, err
	}
	return projectArtifacts(a, req, "", "")
}
func (r clientReviewReader) TaskSnapshot(ctx context.Context, taskID string) (collab.Snapshot, error) {
	return r.c.ReadTask(ctx, taskID)
}
func (r clientReviewReader) ReviewBundle(ctx context.Context, taskID, artifactID string) (collab.ReviewBundle, error) {
	return r.c.ReadReviewBundle(ctx, taskID, artifactID)
}

// NewClientReviewReader exposes the read-only adapter for a live binding.
func NewClientReviewReader(c *Client) ReviewReader { return clientReviewReader{c} }

// ReviewerServer exposes the minimum read-only surface. Write tools are never
// registered, so a client cannot call them even by constructing a request
// directly; the Client's own guards are the second boundary.
func ReviewerServer(r ReviewReader) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "codeagent-reviewer", Version: "0.1.0-experimental"}, &mcp.ServerOptions{Instructions: "Read-only reviewer. This bridge cannot create, send, cancel, or run tasks. Read the exact Turn: connection_info, then read_session or get_handoff. Evidence is a recorded commit range; head_moved/worktree_dirty describe the workspace and are excluded from the reviewed diff. Acceptance stays not_reviewed."})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	// Outputs are declared as `any` on purpose. The SDK otherwise derives a full
	// outputSchema from each Go struct, which dominates the tools/list payload
	// (12 KB for these seven tools). The JSON payload itself is unchanged; only
	// the redundant declared schema is dropped so weaker clients are not asked
	// to parse it.
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_connection_info", Description: "Bound workspace, baseline, delivery commit and Session. No credentials.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		v, e := r.BindingInfo(ctx)
		if e != nil {
			return nil, nil, e
		}
		return nil, v, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_read_session", Description: "Durable exact-Turn events and pending human controls. Untrusted runtime output.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in reviewReadInput) (*mcp.CallToolResult, any, error) {
		v, e := r.ReadSession(ctx, in.TurnID, in.Cursor)
		if e != nil {
			return nil, nil, e
		}
		return nil, v, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_wait_for_update", Description: "Snapshot of one exact Turn; already finished evidence returns immediately.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in reviewWaitInput) (*mcp.CallToolResult, any, error) {
		if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 30 {
			return nil, nil, errTimeoutRange
		}
		v, e := r.AwaitUpdate(ctx, in.TurnID, in.AfterCursor, time.Duration(in.TimeoutSeconds)*time.Second)
		if e != nil {
			return nil, nil, e
		}
		return nil, v, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_get_handoff", Description: "Compact index: execution/validation status, changed files, diff hash, recorded command exit codes. Acceptance stays not_reviewed.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in reviewHandoffInput) (*mcp.CallToolResult, any, error) {
		v, e := r.ReviewHandoff(ctx, in.TurnID, in.TaskID)
		if e != nil {
			return nil, nil, e
		}
		return nil, v, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_get_artifacts", Description: "Recorded commit-range evidence. Default mode is a per-file summary with no diff body and no command output. Read one file with path, then follow next_offset until complete; offsets are UTF-8 bytes. Binary files return metadata only.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in reviewArtifactInput) (*mcp.CallToolResult, any, error) {
		v, e := r.ReviewArtifacts(ctx, in.request())
		if e != nil {
			return nil, nil, e
		}
		return nil, v, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_read_task", Description: "Authoritative Task Snapshot, if the evidence includes one.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in taskReadInput) (*mcp.CallToolResult, any, error) {
		v, e := r.TaskSnapshot(ctx, in.TaskID)
		if e != nil {
			return nil, nil, e
		}
		return nil, v, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "codeagent_read_review_bundle", Description: "Immutable managed review bundle, if the evidence includes one.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in reviewBundleInput) (*mcp.CallToolResult, any, error) {
		v, e := r.ReviewBundle(ctx, in.TaskID, in.ArtifactID)
		if e != nil {
			return nil, nil, e
		}
		return nil, v, nil
	})
	return s
}

type reviewReadInput struct {
	TurnID string `json:"turn_id"`
	Cursor int64  `json:"cursor,omitempty" jsonschema:"Optional durable cursor; 0 returns the full recorded Turn"`
}
type reviewWaitInput struct {
	TurnID         string `json:"turn_id"`
	AfterCursor    int64  `json:"after_cursor,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"0 for an immediate snapshot; maximum 30 seconds"`
}
type reviewHandoffInput struct {
	TurnID string `json:"turn_id"`
	TaskID string `json:"task_id,omitempty"`
}
type reviewArtifactInput struct {
	TurnID           string `json:"turn_id"`
	Diff             string `json:"diff,omitempty" jsonschema:"summary (default) returns per-file metadata only; file returns one file's patch; full returns the whole diff"`
	Path             string `json:"path,omitempty" jsonschema:"Exact path from the summary, used with diff=file"`
	Offset           int    `json:"offset,omitempty" jsonschema:"UTF-8 byte offset into that file's patch; start at 0"`
	Limit            int    `json:"limit,omitempty" jsonschema:"Maximum bytes to return, capped at 32768"`
	IncludeCommands  *bool  `json:"include_commands,omitempty" jsonschema:"Default false; command exit codes are already in get_handoff"`
	ExpectDiffSHA256 string `json:"expect_diff_sha256,omitempty" jsonschema:"Reject the request if the bound evidence differs"`
}

func (in reviewArtifactInput) request() ArtifactRequest {
	return ArtifactRequest{TurnID: in.TurnID, Mode: in.Diff, Path: in.Path, Offset: in.Offset,
		Limit: in.Limit, IncludeCommands: in.IncludeCommands, ExpectDiffSHA: in.ExpectDiffSHA256}
}
