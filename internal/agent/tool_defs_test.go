package agent

import (
	"encoding/json"
	"testing"

	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/git"
)

// TestToolDefinitions checks the two invariants that make the registry the
// single source of truth for tool metadata:
//  1. a visible tool is emitted with a *valid JSON Schema* (not an example), and
//  2. an internal tool (apply_patch) is never advertised to the model.
//
// This test is offline — it never calls the model.
func TestToolDefinitions(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(filesystem.NewListFilesTool(".")); err != nil {
		t.Fatalf("register list_files: %v", err)
	}
	if err := reg.RegisterInternal(git.NewApplyPatchTool(".")); err != nil {
		t.Fatalf("register apply_patch: %v", err)
	}

	defs := toolDefinitions(reg)

	// Invariant 2: apply_patch must not leak to the model.
	for _, d := range defs {
		if d.Function.Name == "apply_patch" {
			t.Fatal("apply_patch is internal and must never be exposed to the model")
		}
	}

	// Invariant 1: list_files is present with a valid JSON Schema object.
	var found bool
	for _, d := range defs {
		if d.Function.Name != "list_files" {
			continue
		}
		found = true

		if d.Type != "function" {
			t.Errorf("list_files type = %q, want %q", d.Type, "function")
		}

		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(d.Function.Parameters, &schema); err != nil {
			t.Fatalf("list_files parameters is not valid JSON Schema: %v", err)
		}
		if schema.Type != "object" {
			t.Errorf("list_files schema type = %q, want %q", schema.Type, "object")
		}
		if _, ok := schema.Properties["path"]; !ok {
			t.Error("list_files schema is missing the \"path\" property")
		}
	}
	if !found {
		t.Fatal("list_files missing from tool definitions")
	}
}
