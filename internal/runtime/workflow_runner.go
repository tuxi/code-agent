package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"code-agent/internal/tools"
)

// ── Conversation workflow tool port (R8) ────────────────────────────

// workflowRunner adapts the runtime's workflow functions (list/detail/save/
// run/delete) to the tools.WorkflowRunner interface consumed by the `workflow`
// conversation tool. It holds the session control plane so Run can dispatch
// cross-session agents exactly like the headless trigger.
type workflowRunner struct {
	control tools.SessionControl
	wsReg   *WorkspaceRegistry
}

// NewWorkflowRunner builds the tools.WorkflowRunner for the `workflow`
// conversation tool. control backs the session tools a cross-workspace run
// needs; wsReg provides per-workspace MCP tools for tool_sequence steps.
func NewWorkflowRunner(control tools.SessionControl, wsReg *WorkspaceRegistry) tools.WorkflowRunner {
	return &workflowRunner{control: control, wsReg: wsReg}
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
	return NewHeadlessRuntime(r.control, r.wsReg).SubmitHeadlessRun(ctx, workspaceRoot, name, input)
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
//
// Step tool names are resolved against the workspace's tool registry before
// compilation: models often emit the bare tool name (market_get_indicator)
// while MCP tools register under their full wire name
// (mcp__okx-trade-mcp__market_get_indicator). A bare name that uniquely
// matches one wire name is rewritten; ambiguous or missing names fail with
// candidates so the model can correct the manifest.
func (r *workflowRunner) SaveToolSequence(ctx context.Context, workspaceRoot, name string, manifest json.RawMessage) (string, error) {
	def, m, err := toolSequenceFromManifestJSON(manifest, name)
	if err != nil {
		return "", err
	}
	if workspaceRoot == "" || name == "" {
		return "", fmt.Errorf("workspaceRoot and name are required")
	}

	// Resolve step tool names against the workspace registry (short → wire).
	if err := r.resolveStepTools(ctx, workspaceRoot, m); err != nil {
		return "", err
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

// resolveStepTools rewrites each step's tool name to its full wire name when
// the step used a bare name. Exact matches pass through; a bare name that
// suffix-matches exactly one mcp__<server>__<tool> wire name is rewritten;
// zero or multiple matches fail with candidates.
func (r *workflowRunner) resolveStepTools(ctx context.Context, workspaceRoot string, m *ToolSequenceManifest) error {
	hr := NewHeadlessRuntime(r.control, r.wsReg)
	ec := hr.BuildHeadlessContext(workspaceRoot, "resolve-tools")
	available := map[string]bool{}
	for _, tool := range ec.ToolRegistry.Visible() {
		if tool != nil {
			available[tool.Name()] = true
		}
	}

	for i := range m.Steps {
		step := &m.Steps[i]
		if step.Tool == "" || available[step.Tool] {
			continue // exact match (or empty — compiler validates later)
		}
		// Bare name: find wire names ending in __<tool>.
		suffix := "__" + step.Tool
		var candidates []string
		for wireName := range available {
			if strings.HasSuffix(wireName, suffix) {
				candidates = append(candidates, wireName)
			}
		}
		switch len(candidates) {
		case 1:
			step.Tool = candidates[0] // unique — rewrite to the wire name
		case 0:
			return fmt.Errorf("tool_sequence: step %d tool %q not found in workspace registry", i+1, step.Tool)
		default:
			sort.Strings(candidates)
			return fmt.Errorf("tool_sequence: step %d tool %q is ambiguous, candidates: %s",
				i+1, step.Tool, strings.Join(candidates, ", "))
		}
	}
	return nil
}

// ResumeTask recovers a suspended/failed/canceled run by task id. It requires
// the started runtime (workers) so the re-enqueued run drives to a terminal
// state asynchronously; tools are re-projected first so a rebuilt runtime
// after daemon restart does not fail with "tool not found".
func (r *workflowRunner) ResumeTask(ctx context.Context, workspaceRoot string, taskID int64, resumeFrom string) error {
	return NewHeadlessRuntime(r.control, r.wsReg).ResumeRun(ctx, workspaceRoot, taskID, resumeFrom)
}

// RunSnapshot returns a run's full snapshot (task status + output + node
// states) by task id — the conversation-side read path for headless runs,
// whose task input carries no workflow_id for the legacy workflow_status
// matching.
func (r *workflowRunner) RunSnapshot(ctx context.Context, workspaceRoot string, taskID int64) (json.RawMessage, error) {
	snap, err := NewWorkflowSnapshotByTaskFunc()(ctx, workspaceRoot, taskID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(snap)
}
