package agent

import (
	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// toolDefinitions converts the registry's model-facing tools into the
// provider-neutral definitions sent with each request.
//
// This is the single place that bridges the tools package to the model
// package. The tools package itself stays unaware of any provider format: a
// tool only describes its own name, description, and input schema, and this
// adapter translates that into what the model expects.
//
// Tools registered with RegisterInternal are withheld from the model —
// Registry.Visible() skips them, so they never reach a request. That is not how
// apply_patch works: it is registered normally and advertised, and its blast
// radius is bounded by the approval layer (auto mode leaves it to a human, see
// docs/p9.1 §12.6) rather than by hiding it.
func toolDefinitions(registry *tools.Registry) []model.ToolDefinition {
	visible := registry.Visible()
	defs := make([]model.ToolDefinition, 0, len(visible))
	for _, t := range visible {
		defs = append(defs, model.ToolDefinition{
			Type: "function",
			Function: model.ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.InputSchema(),
			},
		})
	}
	return defs
}
