package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	domain "github.com/tuxi/flux-workflow/domain"
	workflowruntime "github.com/tuxi/flux-workflow/runtime"
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
		dbPath := filepath.Join(workspaceRoot, ".codeagent", "flux-workflows", "flux-workflows.db")
		if _, err := os.Stat(dbPath); err != nil {
			return nil, fmt.Errorf("workflow not found: %w", err)
		}

		rt, err := workflowruntime.NewLocal(dbPath)
		if err != nil {
			return nil, fmt.Errorf("open workflow db: %w", err)
		}
		defer rt.Shutdown()

		db := rt.DB()
		if db == nil {
			return nil, fmt.Errorf("workflow database unavailable")
		}
		gdb := db.WithContext(ctx)

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

		snapshot := &WorkflowSnapshot{
			WorkflowID: workflowID,
			Task: &WorkflowTaskState{
				ID:       latestTask.ID,
				Status:   string(latestTask.Status),
				Progress: latestTask.Progress,
				Error:    latestTask.ErrorMessage,
			},
		}

		if latestTask.WorkflowVersionID != 0 {
			vid := latestTask.WorkflowVersionID
			snapshot.Task.WorkflowVersionID = &vid
		}
		if len(latestTask.OutputJSON) > 0 {
			snapshot.Task.Output = json.RawMessage(latestTask.OutputJSON)
		}
		if latestTask.InputJSON != nil {
			var input map[string]any
			if json.Unmarshal(latestTask.InputJSON, &input) == nil {
				if goal, ok := input["goal"].(string); ok {
					snapshot.Goal = goal
				}
			}
		}

		// Edges from workflow version definition.
		snapshot.Edges = queryEdges(gdb, latestTask.WorkflowVersionID)

		// Node runtimes — query via raw rows to get output_json as bytes,
		// since domain.NodeRuntime's Output field has no gorm serializer tag
		// and direct Find would leave it nil.
		type nodeRow struct {
			NodeName   string  `gorm:"column:node_name"`
			State      string  `gorm:"column:state"`
			Progress   float64 `gorm:"column:progress"`
			Error      *string `gorm:"column:error"`
			OutputJSON []byte  `gorm:"column:output_json"`
		}
		var rows []nodeRow
		if err := gdb.Table("task_nodes").
			Select("node_name, state, progress, error, output_json").
			Where("task_id = ?", latestTask.ID).
			Order("sort_order ASC").
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
				Progress: r.Progress,
				Error:    errStr,
				Output:   output,
				Terminal: isTermNodeState(domain.NodeState(r.State)),
				Active:   isActiveNodeState(domain.NodeState(r.State)),
			}
			if latestTask.Status == domain.TaskSuspended && domain.NodeState(r.State) == domain.NodeAwaiting {
				ns.Suspended = true
			}
			snapshot.Nodes = append(snapshot.Nodes, ns)
		}

		// Snapshot sequence: max task_events id for this task.
		var maxSeq int64
		_ = gdb.Raw("SELECT COALESCE(MAX(id), 0) FROM task_events WHERE task_id = ?", latestTask.ID).Scan(&maxSeq)
		snapshot.SnapshotSequence = maxSeq

		return snapshot, nil
	}
}

func queryEdges(db *gorm.DB, versionID int64) []WorkflowEdgeDTO {
	if versionID == 0 {
		return nil
	}
	var defJSON []byte
	if err := db.Table("workflow_versions").Select("definition_json").Where("id = ?", versionID).Scan(&defJSON); err != nil {
		return nil
	}
	if len(defJSON) == 0 {
		return nil
	}
	var def struct {
		Nodes []struct {
			Name      string   `json:"name"`
			DependsOn []string `json:"depends_on"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(defJSON, &def); err != nil {
		return nil
	}
	var edges []WorkflowEdgeDTO
	for _, n := range def.Nodes {
		for _, dep := range n.DependsOn {
			edges = append(edges, WorkflowEdgeDTO{From: dep, To: n.Name})
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
