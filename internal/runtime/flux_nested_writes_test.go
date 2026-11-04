package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/tools"
)

// writeEchoExecutor stands in for ExecuteNestedTool: it returns whatever write
// report the test wants the node to have produced.
type writeEchoExecutor struct {
	writes []tools.FileWrite
	nested []tools.FileWrite
}

func (e writeEchoExecutor) ExecuteNestedTool(context.Context, string, string, tools.Tool, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ok", Wrote: e.writes, NestedWrites: e.nested}, nil
}

type statelessTool struct{ name string }

func (t statelessTool) Name() string                 { return t.name }
func (t statelessTool) Description() string          { return "test tool" }
func (t statelessTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t statelessTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "direct"}, nil
}

// A workflow node never reaches the runner's commit step, so its write report
// has to be collected by the Flux tool and leave with the parent result — this
// is what keeps a parent turn from claiming "0 files" after a node edited code.
func TestFluxNodeWritesReachParentResult(t *testing.T) {
	col := &nestedWriteCollector{}
	node := &codeAgentFluxTool{
		tool: statelessTool{name: "edit_file"},
		ec: tools.ExecutionContext{
			NestedExecutor: writeEchoExecutor{
				writes: []tools.FileWrite{{Path: "a.go", Added: 3, Status: tools.WriteModified}},
				nested: []tools.FileWrite{{Path: "b.go", Added: 1, Status: tools.WriteAdded}},
			},
		},
		writes:   col,
		attempts: map[string]int{},
	}

	if _, err := node.Execute(context.Background(), map[string]any{"path": "a.go"}, nil); err != nil {
		t.Fatalf("node execute: %v", err)
	}

	got := col.snapshot()
	if len(got) != 2 {
		t.Fatalf("collected %d writes, want 2: %+v", len(got), got)
	}
	if got[0].Path != "a.go" || got[1].Path != "b.go" {
		t.Fatalf("collected = %+v", got)
	}
}

// A nil collector is what the headless and retry entries pass; it must not panic
// and must not force callers to special-case it.
func TestNestedWriteCollectorNilIsSafe(t *testing.T) {
	var col *nestedWriteCollector
	col.add([]tools.FileWrite{{Path: "a.go", Added: 1}})
	if got := col.snapshot(); got != nil {
		t.Fatalf("snapshot = %+v, want nil", got)
	}
}
