package runtime

import "context"

// ── Workflow run recovery (resume/retry) ────────────────────────────
//
// A run can end up suspended (await binding never resolved) or transiently
// failed. Recovery is the manual escape hatch: the conversation agent or the
// panel re-triggers the run after the underlying cause (child session, MCP
// connection, resolver bug) is addressed. Retry resets the failed/suspended
// subtree and re-enqueues; the workspace runtime's workers drive it to the
// next terminal or suspend point.
//
// The resume entry point is HeadlessRuntime.ResumeRun (headless.go): it first
// ensures the tool projection is registered, because a rebuilt runtime after
// daemon restart has an empty tool registry and re-driving the DAG would fail
// with "tool not found".

// ResumeRunFunc recovers a non-terminal workflow run by task id.
type ResumeRunFunc func(ctx context.Context, workspaceRoot string, taskID int64, resumeFrom string) error
