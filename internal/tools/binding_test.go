package tools

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/preview"
)

type clientFakeTool struct {
	fakeTool
	mode ExecutionMode
}

func (f clientFakeTool) ExecutionMode() ExecutionMode { return f.mode }

func TestRegistryBoundVisibilityAndExecutor(t *testing.T) {
	r := NewRegistry()
	hidden := clientFakeTool{fakeTool: fakeTool{name: "hidden_capture"}, mode: ExecStrictClient}
	if err := r.RegisterBound(hidden, Binding{Name: hidden.Name(), ModelVisibility: VisibilityHidden, Executor: ExecutorClient, Capability: "capture-cap"}); err != nil {
		t.Fatal(err)
	}
	direct := clientFakeTool{fakeTool: fakeTool{name: "direct_capture"}, mode: ExecStrictClient}
	if err := r.RegisterBound(direct, Binding{Name: direct.Name(), ModelVisibility: VisibilityDirect, Executor: ExecutorFlex, Capability: "capture-cap"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("hidden_capture"); !ok {
		t.Fatal("hidden tool must remain runtime-resolvable")
	}
	if got := r.Visible(); len(got) != 1 || got[0].Name() != "direct_capture" {
		t.Fatalf("visible tools = %+v", got)
	}
	binding, ok := r.Binding("hidden_capture")
	if !ok || binding.ModelVisibility != VisibilityHidden || binding.Executor != ExecutorClient || binding.Capability != "capture-cap" {
		t.Fatalf("hidden binding = %+v/%v", binding, ok)
	}
	clone := r.Clone()
	cloneBinding, ok := clone.Binding("hidden_capture")
	if !ok || cloneBinding != binding {
		t.Fatalf("clone binding = %+v/%v", cloneBinding, ok)
	}
}

func TestRegistryRejectsExecutorVisibilityMismatch(t *testing.T) {
	r := NewRegistry()
	clientOnly := clientFakeTool{fakeTool: fakeTool{name: "client_only"}, mode: ExecStrictClient}
	if err := r.RegisterBound(clientOnly, Binding{Name: clientOnly.Name(), ModelVisibility: VisibilityDirect, Executor: ExecutorServer}); err == nil {
		t.Fatal("client-only tool accepted server executor")
	}
	serverOnly := fakeTool{name: "server_only"}
	if err := r.RegisterBound(serverOnly, Binding{Name: serverOnly.Name(), ModelVisibility: VisibilityDirect, Executor: ExecutorClient}); err == nil {
		t.Fatal("server tool accepted client executor")
	}
}

type previewPassthroughTool struct{ fakeTool }

func (f previewPassthroughTool) Execute(context.Context, ExecutionContext, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "ok", Previews: []preview.Draft{{ArtifactKey: "typed-only"}}}, nil
}
