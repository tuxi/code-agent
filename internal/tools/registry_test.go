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
func (f fakeTool) Execute(context.Context, json.RawMessage) (ToolResult, error) {
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
