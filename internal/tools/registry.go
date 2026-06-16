package tools

import (
	"fmt"
	"sort"
)

type Registry struct {
	tools    map[string]Tool
	order    []string        // registration order, for stable iteration
	internal map[string]bool // tools hidden from the model (runtime-only)
}

func NewRegistry() *Registry {
	return &Registry{
		tools:    make(map[string]Tool),
		internal: make(map[string]bool),
	}
}

// Register adds a tool that is visible to the model.
func (r *Registry) Register(tool Tool) error {
	return r.register(tool, false)
}

// RegisterInternal adds a tool the runtime can execute but that is NEVER
// exposed to the model — e.g. apply_patch, whose application must be gated by
// the runtime, not chosen freely by the model. Internal tools are still
// reachable via Get so the runtime can drive them directly.
func (r *Registry) RegisterInternal(tool Tool) error {
	return r.register(tool, true)
}

func (r *Registry) register(tool Tool, internal bool) error {
	if tool == nil {
		return fmt.Errorf("cannot register nil tool")
	}

	name := tool.Name()
	if name == "" {
		return fmt.Errorf("cannot register tool with empty name")
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool already registered: %s", name)
	}

	r.tools[name] = tool
	r.order = append(r.order, name)
	if internal {
		r.internal[name] = true
	}
	return nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	tool, exists := r.tools[name]
	return tool, exists
}

// Visible returns the model-facing tools in registration order. Internal tools
// are excluded. This is the set that should be advertised to the model.
func (r *Registry) Visible() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		if r.internal[name] {
			continue
		}
		out = append(out, r.tools[name])
	}
	return out
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
