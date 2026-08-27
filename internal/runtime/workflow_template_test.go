package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tuxi/flux-workflow/definition"
	workflowruntime "github.com/tuxi/flux-workflow/runtime"
)

// seedFluxRun registers one workflow and submits one task, returning the
// runtime (caller Shutdowns it) and the submitted task id.
func seedFluxRun(t *testing.T, workspaceRoot string) (*workflowruntime.Runtime, int64) {
	t.Helper()
	dir := filepath.Join(workspaceRoot, ".codeagent", "flux-workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := workflowruntime.NewLocal(filepath.Join(dir, "flux-workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown() })

	def := &definition.WorkflowDefinition{
		Name: "source-wf",
		Desc: "source run",
		Nodes: []definition.NodeDefinition{
			{Name: "start", Type: definition.NodeStart},
			{Name: "end", Type: definition.NodeEnd},
		},
		Edges: []definition.EdgeDefinition{
			{From: "start", To: "end", Type: definition.EdgeNormal},
		},
	}
	if err := rt.RegisterWorkflow(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	taskID, err := rt.Submit(context.Background(), "source-wf", map[string]any{
		"goal": "seed goal", "template": "cross_workspace_collaboration_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return rt, taskID
}

func TestSaveTemplateFunc(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".codeagent", "flux-workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a run whose definition is registered and a task is submitted, then
	// capture the source task id by resolving the latest task in the DB.
	rt, taskID := seedFluxRun(t, root)
	_ = rt

	if err := NewSaveTemplateFunc()(context.Background(), root, "friendly-name", "a saved template", taskID); err != nil {
		t.Fatal(err)
	}

	// The friendly name must now appear in the catalog with template markers.
	items, err := NewWorkflowListFunc()(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var found *WorkflowSummary
	for i := range items {
		if items[i].Name == "friendly-name" {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatalf("template not in catalog: %+v", items)
	}

	// The manifest + is_template flag must be persisted on the workflows row.
	db, err := openFluxWorkflowRuntime(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Shutdown()
	var row struct {
		Name         string `gorm:"column:name"`
		ManifestJSON string `gorm:"column:manifest_json"`
		IsTemplate   int    `gorm:"column:is_template"`
	}
	if err := db.DB().Table("workflows").Where("name = ?", "friendly-name").Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.IsTemplate != 1 || row.ManifestJSON == "" {
		t.Fatalf("template markers not persisted: %+v", row)
	}
}

func TestSaveTemplateFuncRequiresSource(t *testing.T) {
	if err := NewSaveTemplateFunc()(context.Background(), t.TempDir(), "x", "", 0); err == nil {
		t.Fatal("expected error for zero source task id")
	}
}
