package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"code-agent/internal/tools"
)

// ── Conversation workflow tool port (R8) ────────────────────────────

// workflowRunner adapts the runtime's workflow functions (list/detail/save/
// run/delete) to the tools.WorkflowRunner interface consumed by the `workflow`
// conversation tool. It holds the session control plane so Run can dispatch
// cross-session agents exactly like the headless trigger.
type workflowRunner struct {
	control tools.SessionControl
}

// NewWorkflowRunner builds the tools.WorkflowRunner for the `workflow`
// conversation tool. control backs the session tools a cross-workspace run
// needs; nil disables Run (and save/delete still work).
func NewWorkflowRunner(control tools.SessionControl) tools.WorkflowRunner {
	return &workflowRunner{control: control}
}

var _ tools.WorkflowRunner = (*workflowRunner)(nil)

func (r *workflowRunner) List(ctx context.Context, workspaceRoot string) (json.RawMessage, error) {
	items, err := NewWorkflowListFunc()(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	return json.Marshal(items)
}

func (r *workflowRunner) Detail(ctx context.Context, workspaceRoot, name string) (json.RawMessage, error) {
	d, err := NewWorkflowDetailFunc()(ctx, workspaceRoot, name)
	if err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

func (r *workflowRunner) SaveTemplate(ctx context.Context, workspaceRoot, name, description string, sourceTaskID int64) error {
	return NewSaveTemplateFunc()(ctx, workspaceRoot, name, description, sourceTaskID)
}

func (r *workflowRunner) Run(ctx context.Context, workspaceRoot, name string, input map[string]any) (int64, error) {
	return NewHeadlessRuntime(r.control).SubmitHeadlessRun(ctx, workspaceRoot, name, input)
}

// Delete soft-deletes a saved template: un-mark it (is_template=0) and drop
// its manifest so it no longer surfaces as a triggerable template. The run
// history rows are kept (they are the audit trail).
func (r *workflowRunner) Delete(ctx context.Context, workspaceRoot, name string) error {
	if name == "" {
		return fmt.Errorf("workflow name is required")
	}
	rt, err := openFluxWorkflowRuntime(workspaceRoot)
	if err != nil {
		return err
	}
	defer rt.Shutdown()
	gdb := rt.DB().WithContext(ctx)

	res := gdb.Table("workflows").Where("name = ?", name).
		Updates(map[string]any{"is_template": 0, "manifest_json": ""})
	if res.Error != nil {
		return fmt.Errorf("delete template %q: %w", name, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("template %q not found", name)
	}
	return nil
}

// SaveToolSequence compiles a tool_sequence manifest and persists it as a
// named template (R9 extract flow): the model produced the manifest from a
// successful conversation's tool trace, this compiles it to a serial DAG and
// registers it under name so it can be triggered like any template.
func (r *workflowRunner) SaveToolSequence(ctx context.Context, workspaceRoot, name string, manifest json.RawMessage) (string, error) {
	def, m, err := toolSequenceFromManifestJSON(manifest, name)
	if err != nil {
		return "", err
	}
	if workspaceRoot == "" || name == "" {
		return "", fmt.Errorf("workspaceRoot and name are required")
	}
	rt, err := openFluxWorkflowRuntime(workspaceRoot)
	if err != nil {
		return "", err
	}
	defer rt.Shutdown()
	gdb := rt.DB().WithContext(ctx)

	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}
	if err := persistTemplateDefinition(ctx, rt, gdb, name, m.Description, def, manifestJSON); err != nil {
		return "", err
	}
	return name, nil
}

// ResumeTask recovers a suspended/failed/canceled run by task id. It requires
// the started runtime (workers) so the re-enqueued run drives to a terminal
// state asynchronously.
func (r *workflowRunner) ResumeTask(ctx context.Context, workspaceRoot string, taskID int64, resumeFrom string) error {
	return NewResumeRunFunc()(ctx, workspaceRoot, taskID, resumeFrom)
}
