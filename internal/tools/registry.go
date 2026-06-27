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

// Clone returns a shallow copy of r with the same tools registered in the same
// order. Modifications to the clone (Register, RegisterInternal) do not affect
// the original. The registered Tool values are shared (not deep-copied).
func (r *Registry) Clone() *Registry {
	c := NewRegistry()
	for _, name := range r.order {
		_ = c.register(r.tools[name], r.internal[name])
	}
	return c
}

// Subset returns a new Registry containing only the named tools from r, in the
// order given. Names not present in r are skipped.
//
// This is how an unattended context — a read-only subagent (8.3) — gets a
// deliberately narrow toolset: it is an ALLOW-LIST, default-deny. A tool is
// included only if explicitly named here, so anything added to the parent
// registry later (a new write tool, an external MCP tool) is excluded by default
// rather than leaking into the subagent. That fail-closed direction is the whole
// point — it must never be inverted into "everything except the side-effecting
// ones", which would silently admit a tool that forgot to mark itself.
func Subset(r *Registry, names ...string) *Registry {
	sub := NewRegistry()
	for _, name := range names {
		if tool, ok := r.tools[name]; ok {
			// Ignore the error: duplicate names in the allow-list just keep the
			// first registration, and the tool is known non-nil with a non-empty
			// name (it came from r).
			_ = sub.register(tool, r.internal[name])
		}
	}
	return sub
}
