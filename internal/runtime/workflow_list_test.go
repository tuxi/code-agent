package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tuxi/flux-workflow/definition"
	workflowruntime "github.com/tuxi/flux-workflow/runtime"
)

// seedFluxTestDB builds a workspace containing a real flux-workflow DB with one
// registered workflow and one submitted (pending) task. Submit persists the
// task row even without starting workers, which is all the catalog readers
// need.
func seedFluxTestDB(t *testing.T, workspaceRoot string) {
	t.Helper()
	dir := filepath.Join(workspaceRoot, ".codeagent", "flux-workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := workflowruntime.NewLocal(filepath.Join(dir, "flux-workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Shutdown()

	def := &definition.WorkflowDefinition{
		Name: "test-wf",
		Desc: "test workflow",
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
	if _, err := rt.Submit(context.Background(), "test-wf", map[string]any{"goal": "seed goal"}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowListFunc(t *testing.T) {
	root := t.TempDir()
	seedFluxTestDB(t, root)

	items, err := NewWorkflowListFunc()(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 workflow, got %d: %+v", len(items), items)
	}
	it := items[0]
	if it.Name != "test-wf" || it.Description != "test workflow" {
		t.Fatalf("workflow summary=%+v", it)
	}
	if it.LatestHash == "" {
		t.Fatal("latest hash not resolved")
	}
	if it.LatestTaskID == 0 || it.LatestStatus == "" {
		t.Fatalf("latest run not resolved: %+v", it)
	}
}

func TestWorkflowListFuncMissingDB(t *testing.T) {
	items, err := NewWorkflowListFunc()(context.Background(), t.TempDir())
	if err == nil {
		t.Fatalf("expected error for empty workspace, got %v", items)
	}
}

func TestWorkflowDetailFunc(t *testing.T) {
	root := t.TempDir()
	seedFluxTestDB(t, root)

	d, err := NewWorkflowDetailFunc()(context.Background(), root, "test-wf")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "test-wf" || len(d.Versions) != 1 || len(d.Runs) != 1 {
		t.Fatalf("detail=%+v", d)
	}
	if d.Versions[0].Hash == "" || d.Runs[0].ID == 0 {
		t.Fatalf("detail fields empty: %+v", d)
	}

	if _, err := NewWorkflowDetailFunc()(context.Background(), root, "missing-wf"); err == nil {
		t.Fatal("expected error for unknown workflow name")
	}
}
