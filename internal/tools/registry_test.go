package tools

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeTool struct {
	name string
	se   bool // declares side effects
}

func (f fakeTool) Name() string                 { return f.name }
func (f fakeTool) Description() string          { return "" }
func (f fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f fakeTool) Execute(context.Context, ExecutionContext, json.RawMessage) (ToolResult, error) {
	return ToolResult{}, nil
}
func (f fakeTool) SideEffects() bool { return f.se }

func TestSubsetIsNameAllowList(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fakeTool{name: "read_file"})
	_ = r.Register(fakeTool{name: "grep"})
	_ = r.Register(fakeTool{name: "edit_file", se: true}) // a write tool

	sub := Subset(r, "read_file", "grep", "does_not_exist")

	if _, ok := sub.Get("read_file"); !ok {
		t.Error("named tool read_file should be included")
	}
	if _, ok := sub.Get("grep"); !ok {
		t.Error("named tool grep should be included")
	}
	// Fail-closed: a tool present in the parent but NOT named is excluded.
	if _, ok := sub.Get("edit_file"); ok {
		t.Error("un-named tool edit_file must be excluded")
	}
	// An unknown name is skipped, not registered as an empty/nil entry.
	if _, ok := sub.Get("does_not_exist"); ok {
		t.Error("unknown name should be skipped")
	}
	if n := len(sub.Visible()); n != 2 {
		t.Errorf("Visible() = %d, want 2", n)
	}
}

func TestSubsetIsIndependentOfParent(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fakeTool{name: "read_file"})
	sub := Subset(r, "read_file")

	// Adding to the parent after the fact must not change the subset.
	_ = r.Register(fakeTool{name: "edit_file", se: true})
	if _, ok := sub.Get("edit_file"); ok {
		t.Error("subset must not see tools added to the parent after Subset()")
	}
}

func TestSubsetEmptyWhenNoNames(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fakeTool{name: "read_file"})
	if n := len(Subset(r).Visible()); n != 0 {
		t.Errorf("empty allow-list should yield no tools, got %d", n)
	}
}

func TestReplace_HotSwapsTool(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fakeTool{name: "web_search"})

	original, _ := r.Get("web_search")
	if original.Name() != "web_search" {
		t.Fatal("expected web_search tool")
	}

	// Replace with a tool that has a distinguishable property.
	replacement := fakeTool{name: "web_search", se: true}
	r.Replace(replacement)

	got, ok := r.Get("web_search")
	if !ok {
		t.Fatal("web_search should still exist after Replace")
	}
	if !got.(fakeTool).se {
		t.Error("Replace did not swap the tool — old instance still registered")
	}
	// Registration order preserved, visible count unchanged.
	if n := len(r.Visible()); n != 1 {
		t.Errorf("Visible() = %d, want 1", n)
	}
}

func TestReplace_NilNoOp(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fakeTool{name: "web_search"})
	r.Replace(nil) // must not panic
	if _, ok := r.Get("web_search"); !ok {
		t.Error("nil Replace must not remove the tool")
	}
}

func TestReplace_UnknownNameNoOp(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fakeTool{name: "web_search"})
	r.Replace(fakeTool{name: "nonexistent"}) // must not register a new tool
	if _, ok := r.Get("nonexistent"); ok {
		t.Error("Replace must not register a tool under a new name")
	}
	if _, ok := r.Get("web_search"); !ok {
		t.Error("existing tool must not be affected by an unknown-name Replace")
	}
}

func TestReplace_EmptyNameToolNoOp(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fakeTool{name: "web_search"})
	r.Replace(fakeTool{name: ""}) // must not panic
	if _, ok := r.Get("web_search"); !ok {
		t.Error("empty-name Replace must not remove the tool")
	}
}
