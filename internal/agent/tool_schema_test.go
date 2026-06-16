package agent

import (
	"encoding/json"
	"testing"

	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/git"
	"code-agent/internal/tools/search"
)

// TestToolSchemasAreWellFormed registers the full model-facing tool set and
// checks each advertised schema:
//   - it parses as an object schema (type == "object"), and
//   - every name listed in "required" actually exists in "properties".
//
// The second check catches the Object(props, "") footgun, which emits
// required:[""] — a required property that does not exist.
//
// This test is offline; it never calls the model.
func TestToolSchemasAreWellFormed(t *testing.T) {
	reg := tools.NewRegistry()
	mustRegister(t, reg, filesystem.NewListFilesTool("."))
	mustRegister(t, reg, filesystem.NewReadFileTool("."))
	mustRegister(t, reg, search.NewGrepTool("."))
	mustRegister(t, reg, git.NewDiffTool("."))

	for _, d := range toolDefinitions(reg) {
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(d.Function.Parameters, &schema); err != nil {
			t.Fatalf("%s: parameters are not valid JSON: %v", d.Function.Name, err)
		}

		if schema.Type != "object" {
			t.Errorf("%s: schema type = %q, want %q", d.Function.Name, schema.Type, "object")
		}

		for _, req := range schema.Required {
			if req == "" {
				t.Errorf("%s: required contains an empty name (likely Object(props, \"\"))", d.Function.Name)
				continue
			}
			if _, ok := schema.Properties[req]; !ok {
				t.Errorf("%s: required %q is not a declared property", d.Function.Name, req)
			}
		}
	}
}

func mustRegister(t *testing.T, reg *tools.Registry, tool tools.Tool) {
	t.Helper()
	if err := reg.Register(tool); err != nil {
		t.Fatalf("register %s: %v", tool.Name(), err)
	}
}
