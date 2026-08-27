package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tuxi/flux-workflow/definition"
	domain "github.com/tuxi/flux-workflow/domain"
	workflowruntime "github.com/tuxi/flux-workflow/runtime"
	"gorm.io/gorm"
)

// ── Save-as-template (R4) ───────────────────────────────────────────

// SaveTemplateFunc persists a run as a user-named, reusable template. The
// run's source manifest (goal/template/agents[]/parallelism — the parent
// task's input_json) is kept as the template's business fact, while the
// compiled definition is re-registered under the friendly name so the run
// seam (POST /v1/workflows/{name}/runs) can trigger it by name.
type SaveTemplateFunc func(ctx context.Context, workspaceRoot, name, description string, sourceTaskID int64) error

// NewSaveTemplateFunc returns a SaveTemplateFunc backed by the workspace's
// durable workflow DB.
func NewSaveTemplateFunc() SaveTemplateFunc {
	return func(ctx context.Context, workspaceRoot, name, description string, sourceTaskID int64) error {
		if workspaceRoot == "" || name == "" || sourceTaskID <= 0 {
			return fmt.Errorf("workspaceRoot, name, and source_task_id are required")
		}
		rt, err := openFluxWorkflowRuntime(workspaceRoot)
		if err != nil {
			return err
		}
		defer rt.Shutdown()
		gdb := rt.DB().WithContext(ctx)

		// The source is the submitted (parent) task: its input_json carries the
		// run's manifest, and its workflow_version_id points at the compiled
		// definition that was actually executed.
		var task domain.Task
		if err := gdb.First(&task, sourceTaskID).Error; err != nil {
			return fmt.Errorf("source task %d not found: %w", sourceTaskID, err)
		}
		if task.WorkflowVersionID == 0 {
			return fmt.Errorf("source task %d has no workflow version", sourceTaskID)
		}
		var version domain.WorkflowVersion
		if err := gdb.First(&version, task.WorkflowVersionID).Error; err != nil {
			return fmt.Errorf("workflow version %d not found: %w", task.WorkflowVersionID, err)
		}
		var wfDef definition.WorkflowDefinition
		if err := json.Unmarshal(version.DefinitionJSON, &wfDef); err != nil {
			return fmt.Errorf("unmarshal definition: %w", err)
		}

		return persistTemplateDefinition(ctx, rt, gdb, name, description, &wfDef, task.InputJSON)
	}
}

// persistTemplateDefinition re-registers a compiled definition under a friendly
// name and stores the source manifest + is_template marker on the workflows
// row. Shared by save-as-template (from a run) and save_tool_sequence (from a
// compiled manifest). RegisterWorkflow upserts by name and auto-versions on
// hash change, so re-saving an existing template iterates it.
func persistTemplateDefinition(ctx context.Context, rt *workflowruntime.Runtime, gdb *gorm.DB, name, description string, def *definition.WorkflowDefinition, manifest []byte) error {
	def.Name = name
	def.Desc = description
	if err := rt.RegisterWorkflow(ctx, def); err != nil {
		return fmt.Errorf("register template: %w", err)
	}

	// Persist the manifest and mark the row as a reusable template. The
	// manifest column is code-agent business data layered on the engine's
	// workflows table (the engine model is unaware of it, so it survives).
	if err := ensureWorkflowTemplateColumns(gdb); err != nil {
		return err
	}
	if len(manifest) == 0 {
		manifest = []byte(`{}`)
	}
	var wf domain.Workflow
	if err := gdb.Where("name = ?", name).First(&wf).Error; err != nil {
		return fmt.Errorf("template %q not found after register: %w", name, err)
	}
	if err := gdb.Table("workflows").Where("id = ?", wf.ID).
		Updates(map[string]any{"manifest_json": string(manifest), "is_template": 1}).Error; err != nil {
		return fmt.Errorf("persist manifest: %w", err)
	}
	return nil
}

// ensureWorkflowTemplateColumns adds the code-agent-only columns to the
// engine's workflows table. Idempotent: existing DBs (created by flux-workflow
// releases without these columns) get them once; later runs no-op.
func ensureWorkflowTemplateColumns(gdb *gorm.DB) error {
	cols := map[string]string{
		"manifest_json": "TEXT",
		"is_template":   "INTEGER DEFAULT 0",
	}
	var existing []struct {
		Name string `gorm:"column:name"`
	}
	if err := gdb.Raw("PRAGMA table_info(workflows)").Scan(&existing).Error; err != nil {
		return fmt.Errorf("inspect workflows schema: %w", err)
	}
	have := map[string]bool{}
	for _, c := range existing {
		have[c.Name] = true
	}
	for col, ddl := range cols {
		if have[col] {
			continue
		}
		if err := gdb.Exec("ALTER TABLE workflows ADD COLUMN " + col + " " + ddl).Error; err != nil {
			return fmt.Errorf("add workflows.%s: %w", col, err)
		}
	}
	return nil
}
