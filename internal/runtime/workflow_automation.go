package runtime

import (
	"context"
	"fmt"

	"code-agent/internal/automation"
	domain "github.com/tuxi/flux-workflow/domain"
)

// ── Automation workflow execution (P3) ──────────────────────────────
//
// Implements automation.WorkflowRunner so an automation firing can trigger a
// saved workflow template directly (zero LLM cost) instead of a prompt turn.

// automationWorkflowRunner adapts the headless run seam + active-run query to
// the automation.WorkflowRunner interface.
type automationWorkflowRunner struct {
	headless *HeadlessRuntime
}

// NewAutomationWorkflowRunner builds the automation.WorkflowRunner used by the
// daemon's automation dispatcher. headless provides the run seam; a nil
// headless/control disables workflow-mode firing (error).
func NewAutomationWorkflowRunner(headless *HeadlessRuntime) automation.WorkflowRunner {
	return &automationWorkflowRunner{headless: headless}
}

var _ automation.WorkflowRunner = (*automationWorkflowRunner)(nil)

// SubmitWorkflowRun triggers a saved template by name and returns the task id.
func (r *automationWorkflowRunner) SubmitWorkflowRun(ctx context.Context, workspaceRoot, workflowName string, input map[string]any) (int64, error) {
	if r.headless == nil {
		return 0, fmt.Errorf("workflow automation: headless runner not wired")
	}
	return r.headless.SubmitHeadlessRun(ctx, workspaceRoot, workflowName, input)
}

// HasActiveWorkflowRun reports whether the workflow has a non-terminal run
// (pending/running/suspended) — the overlap-policy basis.
func (r *automationWorkflowRunner) HasActiveWorkflowRun(ctx context.Context, workspaceRoot, workflowName string) (bool, error) {
	rt, err := openFluxWorkflowRuntime(workspaceRoot)
	if err != nil {
		return false, fmt.Errorf("workflow automation: open store: %w", err)
	}
	defer rt.Shutdown()
	gdb := rt.DB().WithContext(ctx)

	// workflows.name → workflow_id → workflow_versions.id → tasks with a
	// non-terminal status.
	var wf struct {
		ID int64
	}
	if err := gdb.Table("workflows").Where("name = ?", workflowName).Scan(&wf).Error; err != nil || wf.ID == 0 {
		return false, nil // unknown workflow = no active run
	}
	var versionIDs []int64
	if err := gdb.Table("workflow_versions").Where("workflow_id = ?", wf.ID).Pluck("id", &versionIDs).Error; err != nil {
		return false, fmt.Errorf("workflow automation: query versions: %w", err)
	}
	if len(versionIDs) == 0 {
		return false, nil
	}
	var count int64
	if err := gdb.Table("tasks").
		Where("workflow_version_id IN ? AND status IN ?", versionIDs,
			[]domain.TaskStatus{domain.TaskPending, domain.TaskRunning, domain.TaskSuspended}).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("workflow automation: count active tasks: %w", err)
	}
	return count > 0, nil
}
