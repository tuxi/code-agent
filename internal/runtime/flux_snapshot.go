package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	domain "github.com/tuxi/flux-workflow/domain"
	"gorm.io/gorm"
)

// ── Snapshot types (Phase 4 §9.4) ──────────────────────────────────

// WorkflowSnapshotFunc resolves a workflow_id to its current snapshot.
type WorkflowSnapshotFunc func(ctx context.Context, workspaceRoot, workflowID string) (*WorkflowSnapshot, error)

// WorkflowSnapshot is the complete point-in-time state of one workflow run.
type WorkflowSnapshot struct {
	WorkflowID string              `json:"workflow_id"`
	Goal       string              `json:"goal,omitempty"`
	Task       *WorkflowTaskState  `json:"task,omitempty"`
	Nodes      []WorkflowNodeState `json:"nodes"`
	Edges      []WorkflowEdgeDTO   `json:"edges,omitempty"`

	// SnapshotSequence is the highest persisted event sequence at the time the
	// snapshot was taken. Clients replay events with seq > this value.
	SnapshotSequence int64 `json:"snapshot_sequence"`
}

// WorkflowTaskState is the task-level status carried in the snapshot.
type WorkflowTaskState struct {
	ID                int64           `json:"id"`
	Status            string          `json:"status"`
	Progress          float64         `json:"progress"`
	Error             string          `json:"error,omitempty"`
	Output            json.RawMessage `json:"output,omitempty"`
	WorkflowVersionID *int64          `json:"workflow_version_id,omitempty"`
}

// WorkflowNodeState is a single node's current runtime state in the snapshot.
type WorkflowNodeState struct {
	Name      string  `json:"name"`
	State     string  `json:"state"`
	Progress  float64 `json:"progress"`
	Error     string  `json:"error,omitempty"`
	Output    any     `json:"output,omitempty"`
	Terminal  bool    `json:"terminal"`
	Active    bool    `json:"active"`
	Suspended bool    `json:"suspended,omitempty"`
}

// WorkflowEdgeDTO is a directed edge in the DAG topology.
type WorkflowEdgeDTO struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ── Implementation ─────────────────────────────────────────────────

// NewWorkflowSnapshotFunc returns a WorkflowSnapshotFunc that opens the
// durable .db for the given workflow and aggregates task + node states,
// topology, and snapshot sequence.
func NewWorkflowSnapshotFunc() WorkflowSnapshotFunc {
	return func(ctx context.Context, workspaceRoot, workflowID string) (*WorkflowSnapshot, error) {
		if workspaceRoot == "" || workflowID == "" {
			return nil, fmt.Errorf("workspaceRoot and workflowID are required")
		}
		rt, err := openFluxWorkflowRuntime(workspaceRoot)
		if err != nil {
			return nil, err
		}
		defer rt.Shutdown()
		gdb := rt.DB().WithContext(ctx)

		// Find the latest task for this workflow_id. workflow_id is embedded in
		// task.input_json as {"goal":"...","workflow_id":"wf_..."}. We load all
		// tasks and filter in Go since the workspace-scale dataset is small.
		var allTasks []domain.Task
		if err := gdb.Order("id DESC").Find(&allTasks).Error; err != nil {
			return nil, fmt.Errorf("query tasks: %w", err)
		}
		var latestTask *domain.Task
		for i := range allTasks {
			if taskMatchesWorkflow(&allTasks[i], workflowID) {
				latestTask = &allTasks[i]
				break
			}
		}
		if latestTask == nil {
			return nil, fmt.Errorf("workflow %q has no tasks", workflowID)
		}
		return buildWorkflowSnapshot(gdb, workflowID, latestTask)
	}
}

// NewWorkflowSnapshotByTaskFunc returns a WorkflowSnapshotFunc that resolves a
// snapshot by task id — the headless observation key (R2). Headless runs are
// identified by the task id returned from POST /runs, with no conversation to
// derive a workflow id from. The snapshot's WorkflowID is resolved from the
// task's workflow version link when possible; otherwise it is empty.
type WorkflowSnapshotByTaskFunc func(ctx context.Context, workspaceRoot string, taskID int64) (*WorkflowSnapshot, error)

func NewWorkflowSnapshotByTaskFunc() WorkflowSnapshotByTaskFunc {
	return func(ctx context.Context, workspaceRoot string, taskID int64) (*WorkflowSnapshot, error) {
		if workspaceRoot == "" {
			return nil, fmt.Errorf("workspaceRoot is required")
		}
		rt, err := openFluxWorkflowRuntime(workspaceRoot)
		if err != nil {
			return nil, err
		}
		defer rt.Shutdown()
		gdb := rt.DB().WithContext(ctx)

		var task domain.Task
		if err := gdb.First(&task, taskID).Error; err != nil {
			return nil, fmt.Errorf("task %d not found: %w", taskID, err)
		}

		// Resolve the workflow name to use as the snapshot's WorkflowID.
		workflowID := resolveWorkflowName(gdb, task.WorkflowVersionID)
		return buildWorkflowSnapshot(gdb, workflowID, &task)
	}
}

// resolveWorkflowName looks up the workflow name for a given version id.
// Returns "" when the version or workflow row is unreachable.
func resolveWorkflowName(gdb *gorm.DB, versionID int64) string {
	if versionID == 0 {
		return ""
	}
	var ver domain.WorkflowVersion
	if err := gdb.First(&ver, versionID).Error; err != nil {
		return ""
	}
	var wf domain.Workflow
	if err := gdb.First(&wf, ver.WorkflowID).Error; err != nil {
		return ""
	}
	return wf.Name
}

// buildWorkflowSnapshot aggregates task + node states, topology, and snapshot
// sequence into a WorkflowSnapshot. The caller is responsible for finding the
// task first; this extracts the shared aggregation logic out of the original
// NewWorkflowSnapshotFunc so both conversation-scoped and headless variants
// can reuse it.
func buildWorkflowSnapshot(gdb *gorm.DB, workflowID string, task *domain.Task) (*WorkflowSnapshot, error) {
	snapshot := &WorkflowSnapshot{
		WorkflowID: workflowID,
		Task: &WorkflowTaskState{
			ID:       task.ID,
			Status:   string(task.Status),
			Progress: task.Progress,
			Error:    task.ErrorMessage,
		},
	}

	if task.WorkflowVersionID != 0 {
		vid := task.WorkflowVersionID
		snapshot.Task.WorkflowVersionID = &vid
	}
	if len(task.OutputJSON) > 0 {
		snapshot.Task.Output = json.RawMessage(task.OutputJSON)
	}
	if task.InputJSON != nil {
		var input map[string]any
		if json.Unmarshal(task.InputJSON, &input) == nil {
			if goal, ok := input["goal"].(string); ok {
				snapshot.Goal = goal
			}
		}
	}

	// Edges from workflow version definition.
	snapshot.Edges = queryEdges(gdb, task.WorkflowVersionID)

	// Node runtimes — query via raw rows to get output_json as bytes,
	// since domain.NodeRuntime's Output field has no gorm serializer tag
	// and direct Find would leave it nil.
	type nodeRow struct {
		NodeName   string  `gorm:"column:node_name"`
		State      string  `gorm:"column:state"`
		Error      *string `gorm:"column:error"`
		OutputJSON []byte  `gorm:"column:output_json"`
	}
	var rows []nodeRow
	// sort_order and progress columns were added after v1.0.3 — not present
	// in older DBs created by published flux-workflow releases.
	if err := gdb.Table("task_nodes").
		Select("node_name, state, error, output_json").
		Where("task_id = ?", task.ID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query nodes: %w", err)
	}
	for _, r := range rows {
		var output any
		if len(r.OutputJSON) > 0 {
			_ = json.Unmarshal(r.OutputJSON, &output)
		}
		errStr := ""
		if r.Error != nil {
			errStr = *r.Error
		}
		ns := WorkflowNodeState{
			Name:     r.NodeName,
			State:    r.State,
			Progress: 0, // not queried — column added after v1.0.3
			Error:    errStr,
			Output:   output,
			Terminal: isTermNodeState(domain.NodeState(r.State)),
			Active:   isActiveNodeState(domain.NodeState(r.State)),
		}
		if task.Status == domain.TaskSuspended && domain.NodeState(r.State) == domain.NodeAwaiting {
			ns.Suspended = true
		}
		snapshot.Nodes = append(snapshot.Nodes, ns)
	}

	// Snapshot sequence: max task_events id for this task.
	var maxSeq int64
	_ = gdb.Raw("SELECT COALESCE(MAX(id), 0) FROM task_events WHERE task_id = ?", task.ID).Scan(&maxSeq)
	snapshot.SnapshotSequence = maxSeq

	return snapshot, nil
}

func queryEdges(db *gorm.DB, versionID int64) []WorkflowEdgeDTO {
	if versionID == 0 {
		return nil
	}
	var defJSON string
	if err := db.Table("workflow_versions").Select("definition_json").Where("id = ?", versionID).Scan(&defJSON).Error; err != nil {
		return nil
	}
	if defJSON == "" {
		return nil
	}
	var def struct {
		Edges []WorkflowEdgeDTO `json:"edges"`
		Nodes []struct {
			Name      string   `json:"name"`
			DependsOn []string `json:"depends_on"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		return nil
	}
	if len(def.Edges) > 0 {
		return def.Edges
	}
	var edges []WorkflowEdgeDTO
	for _, node := range def.Nodes {
		for _, dependency := range node.DependsOn {
			edges = append(edges, WorkflowEdgeDTO{From: dependency, To: node.Name})
		}
	}
	return edges
}

func isTermNodeState(s domain.NodeState) bool {
	switch s {
	case domain.NodeSuccess, domain.NodeFailed, domain.NodeSkipped, domain.NodeCanceled:
		return true
	}
	return false
}

func isActiveNodeState(s domain.NodeState) bool { return !isTermNodeState(s) }

func taskMatchesWorkflow(task *domain.Task, workflowID string) bool {
	if task == nil || len(task.InputJSON) == 0 {
		return false
	}
	var input struct {
		WorkflowID string `json:"workflow_id"`
	}
	if json.Unmarshal(task.InputJSON, &input) == nil && input.WorkflowID == workflowID {
		return true
	}
	return false
}
