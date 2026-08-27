package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/tools"
)

type fakeRunner struct {
	listErr      error
	detailErr    error
	saveErr      error
	runErr       error
	deleteErr    error
	stsErr       error
	resumeErr    error
	listCalled   bool
	saveCalled   bool
	runCalled    bool
	deleteCalled bool
	stsCalled    bool
	resumeCalled bool
	runTaskID    int64
}

func (f *fakeRunner) List(ctx context.Context, root string) (json.RawMessage, error) {
	f.listCalled = true
	if f.listErr != nil {
		return nil, f.listErr
	}
	return json.RawMessage(`[{"id":1,"name":"wf-a","is_template":true}]`), nil
}
func (f *fakeRunner) Detail(ctx context.Context, root, name string) (json.RawMessage, error) {
	if f.detailErr != nil {
		return nil, f.detailErr
	}
	return json.RawMessage(`{"name":"` + name + `"}`), nil
}
func (f *fakeRunner) SaveTemplate(ctx context.Context, root, name, desc string, sourceTaskID int64) error {
	f.saveCalled = true
	return f.saveErr
}
func (f *fakeRunner) Run(ctx context.Context, root, name string, input map[string]any) (int64, error) {
	f.runCalled = true
	return f.runTaskID, f.runErr
}
func (f *fakeRunner) Delete(ctx context.Context, root, name string) error {
	f.deleteCalled = true
	return f.deleteErr
}
func (f *fakeRunner) SaveToolSequence(ctx context.Context, root, name string, manifest json.RawMessage) (string, error) {
	f.stsCalled = true
	return name, f.stsErr
}
func (f *fakeRunner) ResumeTask(ctx context.Context, root string, taskID int64, resumeFrom string) error {
	f.resumeCalled = true
	return f.resumeErr
}

func execute(t *testing.T, runner tools.WorkflowRunner, raw string) tools.ToolResult {
	t.Helper()
	ec := tools.ExecutionContext{WorkspaceRoot: "/ws", WorkflowRunner: runner}
	result, err := (&WorkflowTool{}).Execute(context.Background(), ec, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("execute %s: %v", raw, err)
	}
	return result
}

func TestWorkflowToolModes(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		runner := &fakeRunner{}
		result := execute(t, runner, `{"mode":"list"}`)
		if !runner.listCalled || len(result.Output) == 0 {
			t.Fatalf("list result=%s", result.Content)
		}
	})

	t.Run("view", func(t *testing.T) {
		result := execute(t, &fakeRunner{}, `{"mode":"view","name":"wf-a"}`)
		var d map[string]string
		if err := json.Unmarshal(result.Output, &d); err != nil || d["name"] != "wf-a" {
			t.Fatalf("view result=%s", result.Content)
		}
	})

	t.Run("save", func(t *testing.T) {
		runner := &fakeRunner{}
		result := execute(t, runner, `{"mode":"save","name":"new-tpl","source_task_id":7}`)
		if !runner.saveCalled || len(result.Output) == 0 {
			t.Fatalf("save result=%s", result.Content)
		}
	})

	t.Run("run", func(t *testing.T) {
		runner := &fakeRunner{runTaskID: 99}
		result := execute(t, runner, `{"mode":"run","name":"new-tpl","input":{"goal":"g"}}`)
		if !runner.runCalled {
			t.Fatal("run not called")
		}
		var out struct {
			TaskID int64 `json:"task_id"`
		}
		if err := json.Unmarshal(result.Output, &out); err != nil || out.TaskID != 99 {
			t.Fatalf("run result=%s", result.Content)
		}
	})

	t.Run("resume", func(t *testing.T) {
		runner := &fakeRunner{}
		result := execute(t, runner, `{"mode":"resume","task_id":42,"resume_from":"wait"}`)
		if !runner.resumeCalled {
			t.Fatal("resume not called")
		}
		var out map[string]any
		if err := json.Unmarshal(result.Output, &out); err != nil || out["task_id"] != float64(42) {
			t.Fatalf("resume result=%s", result.Content)
		}
	})
	t.Run("delete", func(t *testing.T) {
		runner := &fakeRunner{}
		result := execute(t, runner, `{"mode":"delete","name":"old-tpl"}`)
		if !runner.deleteCalled || len(result.Output) == 0 {
			t.Fatalf("delete result=%s", result.Content)
		}
	})
}

func TestWorkflowToolValidation(t *testing.T) {
	runner := &fakeRunner{}
	cases := []struct {
		name  string
		raw   string
		check func(*fakeRunner) bool
	}{
		{"save missing name", `{"mode":"save","source_task_id":1}`, func(*fakeRunner) bool { return true }},
		{"save missing task", `{"mode":"save","name":"x"}`, func(*fakeRunner) bool { return true }},
		{"view missing name", `{"mode":"view"}`, func(*fakeRunner) bool { return true }},
		{"run missing name", `{"mode":"run"}`, func(*fakeRunner) bool { return true }},
		{"delete missing name", `{"mode":"delete"}`, func(*fakeRunner) bool { return true }},
		{"bad mode", `{"mode":"nope"}`, func(*fakeRunner) bool { return true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ec := tools.ExecutionContext{WorkspaceRoot: "/ws", WorkflowRunner: runner}
			if _, err := (&WorkflowTool{}).Execute(context.Background(), ec, json.RawMessage(tc.raw)); err == nil {
				t.Fatalf("expected error for %s", tc.raw)
			}
		})
	}
}

func TestWorkflowToolNoRunner(t *testing.T) {
	ec := tools.ExecutionContext{WorkspaceRoot: "/ws"} // WorkflowRunner nil
	if _, err := (&WorkflowTool{}).Execute(context.Background(), ec, json.RawMessage(`{"mode":"list"}`)); err == nil {
		t.Fatal("expected error when runner is nil")
	}
}

func TestWorkflowToolExtract(t *testing.T) {
	trace := func(ctx context.Context, sessionID string, limit int) ([]tools.TraceStep, error) {
		return []tools.TraceStep{
			{Tool: "mobile_launch_app", Args: map[string]any{"bundle_id": "x"}},
			{Tool: "mobile_type_keys", Result: "typed"},
		}, nil
	}
	ec := tools.ExecutionContext{WorkspaceRoot: "/ws", WorkflowRunner: &fakeRunner{}, SessionID: "sess-1", SessionTrace: trace}
	result, err := (&WorkflowTool{}).Execute(context.Background(), ec, json.RawMessage(`{"mode":"extract"}`))
	if err != nil {
		t.Fatal(err)
	}
	var steps []tools.TraceStep
	if err := json.Unmarshal(result.Output, &steps); err != nil || len(steps) != 2 {
		t.Fatalf("extract result=%s", result.Content)
	}
}

func TestWorkflowToolExtractNoSession(t *testing.T) {
	ec := tools.ExecutionContext{WorkspaceRoot: "/ws", WorkflowRunner: &fakeRunner{}}
	if _, err := (&WorkflowTool{}).Execute(context.Background(), ec, json.RawMessage(`{"mode":"extract"}`)); err == nil {
		t.Fatal("expected error when session id is empty")
	}
}

func TestWorkflowToolSaveToolSequence(t *testing.T) {
	runner := &fakeRunner{}
	manifest := `{"type":"tool_sequence","goal":"g","steps":[{"tool":"read_file","args":{"path":"/a"}}]}`
	ec := tools.ExecutionContext{WorkspaceRoot: "/ws", WorkflowRunner: runner}
	result, err := (&WorkflowTool{}).Execute(context.Background(), ec, json.RawMessage(`{"mode":"save_tool_sequence","name":"tpl","manifest":`+manifest+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if !runner.stsCalled {
		t.Fatal("save_tool_sequence not called")
	}
	var out map[string]string
	if err := json.Unmarshal(result.Output, &out); err != nil || out["name"] != "tpl" {
		t.Fatalf("result=%s", result.Content)
	}
}

func TestWorkflowToolSaveToolSequenceMissingManifest(t *testing.T) {
	ec := tools.ExecutionContext{WorkspaceRoot: "/ws", WorkflowRunner: &fakeRunner{}}
	if _, err := (&WorkflowTool{}).Execute(context.Background(), ec, json.RawMessage(`{"mode":"save_tool_sequence","name":"tpl"}`)); err == nil {
		t.Fatal("expected error when manifest missing")
	}
}

func TestWorkflowToolSaveToolSequenceApprovalGate(t *testing.T) {
	manifest := `{"type":"tool_sequence","goal":"g","steps":[{"tool":"read_file"}]}`
	runner := &fakeRunner{}

	// Approval rejects → cancelled, runner not called.
	reject := tools.ExecutionContext{WorkspaceRoot: "/ws", WorkflowRunner: runner,
		WorkflowPlanApproval: func(_, _, _ string) bool { return false }}
	result, err := (&WorkflowTool{}).Execute(context.Background(), reject, json.RawMessage(`{"mode":"save_tool_sequence","name":"tpl","manifest":`+manifest+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if runner.stsCalled {
		t.Fatal("runner called despite approval rejection")
	}
	if result.Content == "" {
		t.Fatal("expected a cancelled notice")
	}

	// Approval accepts → saved.
	runner2 := &fakeRunner{}
	accept := tools.ExecutionContext{WorkspaceRoot: "/ws", WorkflowRunner: runner2,
		WorkflowPlanApproval: func(_, _, _ string) bool { return true }}
	if _, err := (&WorkflowTool{}).Execute(context.Background(), accept, json.RawMessage(`{"mode":"save_tool_sequence","name":"tpl","manifest":`+manifest+`}`)); err != nil {
		t.Fatal(err)
	}
	if !runner2.stsCalled {
		t.Fatal("runner not called despite approval")
	}
}
