package tools

import (
	"context"
	"encoding/json"

	flux "flux"
	fluxtool "flux/tool"
)

// FluxWorkflowAdapter wraps flux.WorkflowTool as a code-agent tools.Tool.
// This enables Tool embedding mode — same process, lower latency, shared LLM provider.
type FluxWorkflowAdapter struct {
	wt *flux.WorkflowTool
}

// NewFluxWorkflowAdapter creates a code-agent compatible tool wrapper.
func NewFluxWorkflowAdapter(wt *flux.WorkflowTool) *FluxWorkflowAdapter {
	return &FluxWorkflowAdapter{wt: wt}
}

func (a *FluxWorkflowAdapter) Name() string             { return a.wt.Name() }
func (a *FluxWorkflowAdapter) Description() string      { return a.wt.Description() }

func (a *FluxWorkflowAdapter) InputSchema() json.RawMessage {
	ds := a.wt.InputSchema()
	// Convert flux DataSchema → JSON Schema
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{},
	}
	props := schema["properties"].(map[string]any)
	for name, field := range ds.Fields {
		prop := map[string]any{"type": field.Type}
		if field.Desc != "" {
			prop["description"] = field.Desc
		}
		props[name] = prop
		if field.Required {
			required, _ := schema["required"].([]string)
			schema["required"] = append(required, name)
		}
	}
	b, _ := json.Marshal(schema)
	return b
}

func (a *FluxWorkflowAdapter) Execute(ctx context.Context, ec ExecutionContext, input json.RawMessage) (ToolResult, error) {
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return ToolResult{Content: "invalid input: " + err.Error()}, nil
	}

	result, err := a.wt.Execute(ctx, args, nil)
	if err != nil {
		return ToolResult{Content: "flux error: " + err.Error()}, nil
	}

	b, _ := json.MarshalIndent(result.Data, "", "  ")
	return ToolResult{Content: string(b)}, nil
}

// compile-time check
var _ Tool = (*FluxWorkflowAdapter)(nil)

// Ensure json import is used by the init if needed for the marshal fallback.
var _ = json.Marshal

// Ensure fluxtool import used.
var _ = fluxtool.SyncExecution
