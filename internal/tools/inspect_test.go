package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// inspectingTool implements both Tool and Inspector for testing.
type inspectingTool struct {
	name string
	// inspectErr is the error Inspect returns (nil = pass).
	inspectErr error
}

func (t *inspectingTool) Name() string                      { return t.name }
func (t *inspectingTool) Description() string               { return "test tool" }
func (t *inspectingTool) InputSchema() json.RawMessage       { return nil }
func (t *inspectingTool) Execute(_ context.Context, _ ExecutionContext, _ json.RawMessage) (ToolResult, error) {
	return ToolResult{}, nil
}
func (t *inspectingTool) Inspect(_ json.RawMessage, _ string) error { return t.inspectErr }

// plainTool implements only Tool (not Inspector).
type plainTool struct {
	name string
}

func (t *plainTool) Name() string                      { return t.name }
func (t *plainTool) Description() string               { return "plain tool" }
func (t *plainTool) InputSchema() json.RawMessage       { return nil }
func (t *plainTool) Execute(_ context.Context, _ ExecutionContext, _ json.RawMessage) (ToolResult, error) {
	return ToolResult{}, nil
}

func TestHasInspector(t *testing.T) {
	t.Run("tool implementing Inspector returns true", func(t *testing.T) {
		if !HasInspector(&inspectingTool{name: "with_inspect"}) {
			t.Error("HasInspector should return true for a tool that implements Inspector")
		}
	})

	t.Run("tool not implementing Inspector returns false", func(t *testing.T) {
		if HasInspector(&plainTool{name: "no_inspect"}) {
			t.Error("HasInspector should return false for a tool that does not implement Inspector")
		}
	})

	t.Run("nil tool returns false without panic", func(t *testing.T) {
		var tool Tool
		if HasInspector(tool) {
			t.Error("HasInspector should return false for nil")
		}
	})
}

func TestInspectorInterfaceSatisfaction(t *testing.T) {
	// Compile-time check: these must satisfy the stated interfaces.
	var _ Tool = &inspectingTool{name: "test"}
	var _ Inspector = &inspectingTool{name: "test"}
	var _ Tool = &plainTool{name: "test"}
}

func TestInspectorInspectCalled(t *testing.T) {
	t.Run("Inspect passes when returning nil", func(t *testing.T) {
		tool := &inspectingTool{name: "passing", inspectErr: nil}
		if err := tool.Inspect(json.RawMessage(`{}`), "/tmp/ws"); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("Inspect blocks when returning non-nil", func(t *testing.T) {
		wantErr := errors.New("blocked: sensitive path")
		tool := &inspectingTool{name: "blocking", inspectErr: wantErr}
		err := tool.Inspect(json.RawMessage(`{}`), "/tmp/ws")
		if err == nil {
			t.Fatal("expected error from Inspect, got nil")
		}
		if err.Error() != wantErr.Error() {
			t.Errorf("expected %q, got %q", wantErr.Error(), err.Error())
		}
	})
}
