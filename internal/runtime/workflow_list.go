package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	domain "github.com/tuxi/flux-workflow/domain"
	workflowruntime "github.com/tuxi/flux-workflow/runtime"
	"gorm.io/gorm"
)

// ── Catalog types (R1: workflow panel directory) ────────────────────

// WorkflowSummary is one catalog row for the panel directory: the workflow's
// metadata plus its latest definition hash and latest run state. The latest
// run state is what lets the panel show red/green status without opening any
// run.
type WorkflowSummary struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	LatestHash   string `json:"latest_hash,omitempty"`
	LatestTaskID int64  `json:"latest_task_id,omitempty"`
	LatestStatus string `json:"latest_status,omitempty"`
	LatestError  string `json:"latest_error,omitempty"`
}

// VersionSummary is one immutable definition version.
type VersionSummary struct {
	ID        int64  `json:"id"`
	Version   int64  `json:"version"`
	Hash      string `json:"hash"`
	CreatedAt string `json:"created_at"`
}

// TaskSummary is one run of a workflow.
type TaskSummary struct {
	ID        int64   `json:"id"`
	Status    string  `json:"status"`
	Progress  float64 `json:"progress"`
	Error     string  `json:"error,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// WorkflowDetail is the full history of one named workflow: its definition
// versions and every run that linked to one of them.
type WorkflowDetail struct {
	ID          int64            `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Versions    []VersionSummary `json:"versions"`
	Runs        []TaskSummary    `json:"runs"`
}

// WorkflowListFunc resolves a workspace's workflow catalog. It is session-
// independent: the panel directory (R1) must render without any conversation,
// so the durable DB path is derived from workspaceRoot alone.
type WorkflowListFunc func(ctx context.Context, workspaceRoot string) ([]WorkflowSummary, error)

// WorkflowDetailFunc resolves one named workflow's versions and run history.
type WorkflowDetailFunc func(ctx context.Context, workspaceRoot, workflowName string) (*WorkflowDetail, error)

// ── Shared DB access ────────────────────────────────────────────────

// openFluxWorkflowRuntime opens the workspace's durable workflow DB and
// returns the runtime. The caller must Shutdown it when done; Shutdown closes
// the underlying SQLite connection, so queries must finish first.
func openFluxWorkflowRuntime(workspaceRoot string) (*workflowruntime.Runtime, error) {
	if workspaceRoot == "" {
		return nil, fmt.Errorf("workspaceRoot is required")
	}
	dbPath := filepath.Join(workspaceRoot, ".codeagent", "flux-workflows", "flux-workflows.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("workflow not found: %w", err)
	}
	rt, err := workflowruntime.NewLocal(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open workflow db: %w", err)
	}
	return rt, nil
}

// workflowCatalogIndexes pre-loads versions and tasks into lookup maps so the
// catalog and detail queries are a handful of bulk reads instead of N+1. The
// workspace-scale dataset is small, matching flux_snapshot.go's assumption.
func workflowCatalogIndexes(gdb *gorm.DB) (
	latestVersion map[int64]domain.WorkflowVersion,
	latestTask map[int64]domain.Task,
	err error,
) {
	var versions []domain.WorkflowVersion
	if err := gdb.Order("id DESC").Find(&versions).Error; err != nil {
		return nil, nil, fmt.Errorf("query workflow_versions: %w", err)
	}
	var tasks []domain.Task
	if err := gdb.Order("id DESC").Find(&tasks).Error; err != nil {
		return nil, nil, fmt.Errorf("query tasks: %w", err)
	}

	latestVersion = make(map[int64]domain.WorkflowVersion, len(versions))
	versionToWorkflow := make(map[int64]int64, len(versions))
	for _, v := range versions {
		versionToWorkflow[v.ID] = v.WorkflowID
		if _, ok := latestVersion[v.WorkflowID]; !ok {
			latestVersion[v.WorkflowID] = v // first hit in id DESC = newest
		}
	}

	// A run belongs to a workflow through its workflow_version_id. Tasks with
	// no version link (e.g. executed via Run with an unregistered definition)
	// have no catalog home and are skipped, like the snapshot's workflow_id
	// matching.
	latestTask = make(map[int64]domain.Task)
	for _, t := range tasks {
		wfID, ok := versionToWorkflow[t.WorkflowVersionID]
		if !ok {
			continue
		}
		if _, seen := latestTask[wfID]; !seen {
			latestTask[wfID] = t // first hit in id DESC = latest
		}
	}
	return latestVersion, latestTask, nil
}

// ── Implementations ─────────────────────────────────────────────────

// NewWorkflowListFunc returns a WorkflowListFunc that opens the workspace's
// durable workflow DB and aggregates the catalog.
func NewWorkflowListFunc() WorkflowListFunc {
	return func(ctx context.Context, workspaceRoot string) ([]WorkflowSummary, error) {
		rt, err := openFluxWorkflowRuntime(workspaceRoot)
		if err != nil {
			return nil, err
		}
		defer rt.Shutdown()
		gdb := rt.DB().WithContext(ctx)

		var wfs []domain.Workflow
		if err := gdb.Order("id ASC").Find(&wfs).Error; err != nil {
			return nil, fmt.Errorf("query workflows: %w", err)
		}

		latestVersion, latestTask, err := workflowCatalogIndexes(gdb)
		if err != nil {
			return nil, err
		}

		items := make([]WorkflowSummary, 0, len(wfs))
		for _, wf := range wfs {
			item := WorkflowSummary{ID: wf.ID, Name: wf.Name, Description: wf.Description}
			if v, ok := latestVersion[wf.ID]; ok {
				item.LatestHash = v.Hash
			}
			if t, ok := latestTask[wf.ID]; ok {
				item.LatestTaskID = t.ID
				item.LatestStatus = string(t.Status)
				item.LatestError = t.ErrorMessage
			}
			items = append(items, item)
		}
		return items, nil
	}
}

// NewWorkflowDetailFunc returns a WorkflowDetailFunc that resolves one named
// workflow's versions and run history.
func NewWorkflowDetailFunc() WorkflowDetailFunc {
	return func(ctx context.Context, workspaceRoot, workflowName string) (*WorkflowDetail, error) {
		if workflowName == "" {
			return nil, fmt.Errorf("workflow name is required")
		}
		rt, err := openFluxWorkflowRuntime(workspaceRoot)
		if err != nil {
			return nil, err
		}
		defer rt.Shutdown()
		gdb := rt.DB().WithContext(ctx)

		var wf domain.Workflow
		if err := gdb.Where("name = ?", workflowName).First(&wf).Error; err != nil {
			return nil, fmt.Errorf("workflow %q not found: %w", workflowName, err)
		}

		var versions []domain.WorkflowVersion
		if err := gdb.Where("workflow_id = ?", wf.ID).Order("id DESC").Find(&versions).Error; err != nil {
			return nil, fmt.Errorf("query workflow_versions: %w", err)
		}

		versionIDs := make([]int64, 0, len(versions))
		for _, v := range versions {
			versionIDs = append(versionIDs, v.ID)
		}
		var tasks []domain.Task
		if len(versionIDs) > 0 {
			if err := gdb.Where("workflow_version_id IN ?", versionIDs).Order("id DESC").Find(&tasks).Error; err != nil {
				return nil, fmt.Errorf("query tasks: %w", err)
			}
		}

		d := &WorkflowDetail{ID: wf.ID, Name: wf.Name, Description: wf.Description}
		for _, v := range versions {
			d.Versions = append(d.Versions, VersionSummary{
				ID: v.ID, Version: v.Version, Hash: v.Hash,
				CreatedAt: v.CreatedAt.Format(time.RFC3339),
			})
		}
		for _, t := range tasks {
			d.Runs = append(d.Runs, TaskSummary{
				ID: t.ID, Status: string(t.Status), Progress: t.Progress,
				Error: t.ErrorMessage, CreatedAt: t.CreatedAt.Format(time.RFC3339),
			})
		}
		return d, nil
	}
}
