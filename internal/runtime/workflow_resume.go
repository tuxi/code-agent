package runtime

import (
	"context"
	"fmt"
)

// ── Workflow run recovery (resume/retry) ────────────────────────────
//
// A run can end up suspended (await binding never resolved) or transiently
// failed. Recovery is the manual escape hatch: the conversation agent or the
// panel re-triggers the run after the underlying cause (child session, MCP
// connection, resolver bug) is addressed. Retry resets the failed/suspended
// subtree and re-enqueues; the workspace runtime's workers drive it to the
// next terminal or suspend point.

// ResumeRunFunc recovers a non-terminal workflow run by task id.
type ResumeRunFunc func(ctx context.Context, workspaceRoot string, taskID int64, resumeFrom string) error

// NewResumeRunFunc returns a ResumeRunFunc backed by the workspace's started
// runtime (task + await-poll workers), so the recovered run executes
// asynchronously after re-enqueue.
func NewResumeRunFunc() ResumeRunFunc {
	return func(ctx context.Context, workspaceRoot string, taskID int64, resumeFrom string) error {
		if workspaceRoot == "" || taskID <= 0 {
			return fmt.Errorf("workspaceRoot and task_id are required")
		}
		rt, err := getOrCreateRuntime(ctx, workspaceRoot)
		if err != nil {
			return fmt.Errorf("resume run: %w", err)
		}
		// flux-workflow Retry validates status (failed/canceled/suspended only)
		// and re-enqueues; empty resumeFrom auto-collects failed root nodes.
		if err := rt.Retry(ctx, taskID, resumeFrom, nil); err != nil {
			return fmt.Errorf("resume run: %w", err)
		}
		return nil
	}
}
