package tools

import (
	"fmt"
	"sort"
)

type ModelVisibility string

const (
	VisibilityDirect ModelVisibility = "direct"
	VisibilityHidden ModelVisibility = "hidden"
)

type Executor string

const (
	ExecutorServer Executor = "server"
	ExecutorClient Executor = "client"
	ExecutorFlex   Executor = "flex"
)

// Binding is the runtime-owned exposure and dispatch metadata for one registered
// tool. Capability is never accepted from the model or client tool result; the
// capability runtime stamps it from the active manifest binding.
type Binding struct {
	Name            string
	ModelVisibility ModelVisibility
	Executor        Executor
	Capability      string
}

func (b Binding) Normalize() Binding {
	if b.ModelVisibility == "" {
		b.ModelVisibility = VisibilityDirect
	}
	if b.Executor == "" {
		b.Executor = ExecutorServer
	}
	return b
}

func (b Binding) Validate() error {
	b = b.Normalize()
	if b.Name == "" {
		return fmt.Errorf("tool binding name is required")
	}
	if b.ModelVisibility != VisibilityDirect && b.ModelVisibility != VisibilityHidden {
		return fmt.Errorf("unsupported model_visibility %q", b.ModelVisibility)
	}
	if b.Executor != ExecutorServer && b.Executor != ExecutorClient && b.Executor != ExecutorFlex {
		return fmt.Errorf("unsupported executor %q", b.Executor)
	}
	return nil
}

type Registry struct {
	tools    map[string]Tool
	order    []string
	internal map[string]bool
	bindings map[string]Binding
}

func NewRegistry() *Registry {
	return &Registry{
		tools:    make(map[string]Tool),
		internal: make(map[string]bool),
		bindings: make(map[string]Binding),
	}
}

// Register adds a tool that is visible to the model. Legacy tools default to
// direct/server; client proxies keep their strict-client execution contract.
func (r *Registry) Register(tool Tool) error {
	binding := Binding{Name: toolName(tool), ModelVisibility: VisibilityDirect, Executor: ExecutorServer}
	if tool != nil {
		if mode, ok := tool.(ClientTool); ok {
			switch mode.ExecutionMode() {
			case ExecStrictClient:
				binding.Executor = ExecutorClient
			case ExecFlex:
				binding.Executor = ExecutorFlex
			}
		}
	}
	return r.RegisterBound(tool, binding)
}

// RegisterInternal adds a tool the runtime can execute but that is never offered
// to the model.
func (r *Registry) RegisterInternal(tool Tool) error {
	binding := Binding{Name: toolName(tool), ModelVisibility: VisibilityHidden, Executor: ExecutorServer}
	if tool != nil {
		if mode, ok := tool.(ClientTool); ok {
			switch mode.ExecutionMode() {
			case ExecStrictClient:
				binding.Executor = ExecutorClient
			case ExecFlex:
				binding.Executor = ExecutorFlex
			}
		}
	}
	return r.RegisterBound(tool, binding)
}

// RegisterBound registers a tool with explicit model visibility and executor
// dimensions. Hidden tools are excluded from Registry.Visible but remain
// resolvable by runtime-owned dispatch code.
func (r *Registry) RegisterBound(tool Tool, binding Binding) error {
	if tool == nil {
		return fmt.Errorf("cannot register nil tool")
	}
	name := tool.Name()
	if name == "" {
		return fmt.Errorf("cannot register tool with empty name")
	}
	binding.Name = name
	binding = binding.Normalize()
	if err := binding.Validate(); err != nil {
		return err
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool already registered: %s", name)
	}
	if err := validateBindingForTool(tool, binding); err != nil {
		return err
	}
	r.tools[name] = tool
	r.order = append(r.order, name)
	r.bindings[name] = binding
	if binding.ModelVisibility == VisibilityHidden {
		r.internal[name] = true
	}
	return nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	tool, exists := r.tools[name]
	return tool, exists
}

func (r *Registry) Binding(name string) (Binding, bool) {
	binding, ok := r.bindings[name]
	return binding, ok
}

// SetBinding changes runtime-owned exposure metadata for an existing tool. It
// does not alter the tool implementation.
func (r *Registry) SetBinding(name string, binding Binding) error {
	if _, ok := r.tools[name]; !ok {
		return fmt.Errorf("tool not registered: %s", name)
	}
	binding.Name = name
	binding = binding.Normalize()
	if err := binding.Validate(); err != nil {
		return err
	}
	if err := validateBindingForTool(r.tools[name], binding); err != nil {
		return err
	}
	r.bindings[name] = binding
	if binding.ModelVisibility == VisibilityHidden {
		r.internal[name] = true
	} else {
		delete(r.internal, name)
	}
	return nil
}

// Visible returns the model-facing tools in registration order. Internal tools
// are excluded. This is the set advertised to the model.
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

// Replace swaps in a new implementation for the named tool. It keeps the
// original registration order and binding metadata.
func (r *Registry) Replace(tool Tool) {
	if tool == nil {
		return
	}
	name := tool.Name()
	if name == "" {
		return
	}
	if _, exists := r.tools[name]; !exists {
		return
	}
	r.tools[name] = tool
}

// Clone returns a shallow copy of r with the same tools and binding metadata.
func (r *Registry) Clone() *Registry {
	c := NewRegistry()
	for _, name := range r.order {
		_ = c.registerCopy(r.tools[name], r.internal[name], r.bindings[name])
	}
	return c
}

func (r *Registry) registerCopy(tool Tool, internal bool, binding Binding) error {
	if err := cRegister(r, tool, binding); err != nil {
		return err
	}
	if internal {
		r.internal[tool.Name()] = true
	}
	return nil
}

func cRegister(r *Registry, tool Tool, binding Binding) error {
	name := tool.Name()
	r.tools[name] = tool
	r.order = append(r.order, name)
	r.bindings[name] = binding
	return nil
}

// Subset returns a new Registry containing only the named tools from r, in the
// order given. Names not present in r are skipped.
func Subset(r *Registry, names ...string) *Registry {
	sub := NewRegistry()
	for _, name := range names {
		if tool, ok := r.tools[name]; ok {
			_ = sub.registerCopy(tool, r.internal[name], r.bindings[name])
		}
	}
	return sub
}

func validateBindingForTool(tool Tool, binding Binding) error {
	name := tool.Name()
	mode, hasMode := tool.(ClientTool)
	if binding.Executor == ExecutorClient && !hasMode {
		return fmt.Errorf("tool %s has no client execution implementation", name)
	}
	if hasMode {
		switch mode.ExecutionMode() {
		case ExecStrictClient:
			if binding.Executor == ExecutorServer {
				return fmt.Errorf("client-only tool %s cannot use server executor", name)
			}
		case ExecStrictServer:
			if binding.Executor == ExecutorClient {
				return fmt.Errorf("server-only tool %s cannot use client executor", name)
			}
		}
	}
	return nil
}

func toolName(tool Tool) string {
	if tool == nil {
		return ""
	}
	return tool.Name()
}
