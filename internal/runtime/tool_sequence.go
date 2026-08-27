package runtime

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/tuxi/flux-workflow/definition"
)

// ── tool_sequence compiler (R9) ─────────────────────────────────────
//
// A tool_sequence template is a deterministic, serial chain of tool calls
// extracted from a successful free-form conversation (R9). Unlike v1/v2
// cross-workspace templates it has no Map fan-out and no dispatch nodes —
// every step is one tool invocation, parameterized by the template's inputs.
// Task classification boundary (§2.1): only deterministic steps qualify;
// exploration loops (screenshot → perceive → decide) must stay as agent
// nodes in v1/v2 templates, not tool_sequence steps.

// ToolSequenceManifest is the LLM-produced intermediate representation of a
// tool_sequence template. Stored in workflows.manifest_json.
type ToolSequenceManifest struct {
	Type        string              `json:"type"` // "tool_sequence"
	Goal        string              `json:"goal"`
	Description string              `json:"description,omitempty"`
	Inputs      []ToolSequenceInput `json:"inputs,omitempty"`
	Steps       []ToolSequenceStep  `json:"steps"`
}

// ToolSequenceInput declares one run-time variable referenced by steps.
type ToolSequenceInput struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// ToolSequenceStep is one deterministic tool call.
type ToolSequenceStep struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// varRefPattern matches an entire string that is a pure {{name}} variable
// reference. R9 v1 only supports whole-value references (no prefix/suffix
// text); mixed values must be split by the compiler prompt into static +
// variable parts.
var varRefPattern = regexp.MustCompile(`^\{\{([a-zA-Z0-9_]+)\}\}$`)

// CompileToolSequence compiles a tool_sequence manifest into a serial DAG:
//
//	start → step_1 → step_2 → … → step_N → end
//
// Each step is a NodeTool; a {{var}} whole-value arg becomes an input_mapping
// expression (input.var) so it is resolved at run time from the submitted
// input, everything else goes into the node's static Config. The returned
// definition can be persisted via RegisterWorkflow and triggered by name.
func CompileToolSequence(m *ToolSequenceManifest) (*definition.WorkflowDefinition, error) {
	if m == nil || m.Goal == "" {
		return nil, fmt.Errorf("tool_sequence: goal is required")
	}
	if len(m.Steps) == 0 {
		return nil, fmt.Errorf("tool_sequence: at least one step is required")
	}

	// Validate that every referenced {{var}} is declared in inputs.
	declared := map[string]bool{}
	for _, in := range m.Inputs {
		declared[in.Name] = true
	}
	for i, step := range m.Steps {
		if step.Tool == "" {
			return nil, fmt.Errorf("tool_sequence: step %d missing tool", i+1)
		}
		for k, v := range step.Args {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if mm := varRefPattern.FindStringSubmatch(s); mm != nil && !declared[mm[1]] {
				return nil, fmt.Errorf("tool_sequence: step %d arg %q references undeclared input %q", i+1, k, mm[1])
			}
		}
	}

	nodes := []definition.NodeDefinition{
		{Name: "start", Label: "Start", Type: definition.NodeStart},
	}
	edges := []definition.EdgeDefinition{
		{From: "start", To: "step_1", Type: definition.EdgeNormal},
	}
	prev := "start"
	for i, step := range m.Steps {
		name := fmt.Sprintf("step_%d", i+1)
		config := map[string]any{"tool": step.Tool}
		inputMapping := map[string]string{}
		for k, v := range step.Args {
			if s, ok := v.(string); ok {
				if mm := varRefPattern.FindStringSubmatch(s); mm != nil {
					inputMapping[k] = "input." + mm[1]
					continue
				}
			}
			config[k] = v
		}
		nodes = append(nodes, definition.NodeDefinition{
			Name: name, Label: "Step " + step.Tool, Type: definition.NodeTool,
			Config: config, InputMapping: inputMapping,
		})
		edges = append(edges, definition.EdgeDefinition{From: prev, To: name, Type: definition.EdgeNormal})
		prev = name
	}
	nodes = append(nodes, definition.NodeDefinition{Name: "end", Label: "End", Type: definition.NodeEnd})
	edges = append(edges, definition.EdgeDefinition{From: prev, To: "end", Type: definition.EdgeNormal})

	// The workflow name is provisional; the save path renames it to the
	// user-chosen template name before RegisterWorkflow.
	return &definition.WorkflowDefinition{
		Name:  m.Goal,
		Desc:  m.Description,
		Nodes: nodes,
		Edges: edges,
		Output: definition.OutputDefinition{
			ResultType: "tool_sequence",
			Extras: map[string]string{
				"result": "nodes." + prev + ".output.content",
			},
		},
	}, nil
}

// toolSequenceFromManifestJSON parses and compiles a tool_sequence manifest
// carried as JSON (used by the workflow tool's save_tool_sequence mode).
func toolSequenceFromManifestJSON(raw json.RawMessage, name string) (*definition.WorkflowDefinition, *ToolSequenceManifest, error) {
	var m ToolSequenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("tool_sequence: parse manifest: %w", err)
	}
	def, err := CompileToolSequence(&m)
	if err != nil {
		return nil, nil, err
	}
	def.Name = name
	return def, &m, nil
}
