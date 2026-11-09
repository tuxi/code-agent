package capability

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"sync"

	"code-agent/internal/tools"

	"gopkg.in/yaml.v3"
)

// Manifest is the repository-local capability package declaration. Unknown
// fields are rejected so a misspelled required declaration fails closed.
type Manifest struct {
	Name               string        `json:"name" yaml:"name"`
	Version            string        `json:"version" yaml:"version"`
	Description        string        `json:"description" yaml:"description"`
	Skills             []string      `json:"skills,omitempty" yaml:"skills,omitempty"`
	ToolBindings       []ToolBinding `json:"tool_bindings,omitempty" yaml:"tool_bindings,omitempty"`
	Extensions         []string      `json:"extensions,omitempty" yaml:"extensions,omitempty"`
	OptionalMCP        []string      `json:"optional_mcp,omitempty" yaml:"optional_mcp,omitempty"`
	ClientCapabilities []string      `json:"client_capabilities,omitempty" yaml:"client_capabilities,omitempty"`
	EvidenceKinds      []string      `json:"evidence_kinds,omitempty" yaml:"evidence_kinds,omitempty"`
}

type ToolBinding struct {
	Name            string                `json:"name" yaml:"name"`
	ModelVisibility tools.ModelVisibility `json:"model_visibility" yaml:"model_visibility"`
	Executor        tools.Executor        `json:"executor" yaml:"executor"`
}

type Activation struct {
	Manifest                  Manifest
	Source                    string
	MissingClientCapabilities []string
}

type Registry struct {
	mu       sync.RWMutex
	active   map[string]Activation
	bindings map[string]map[string]ToolBinding // tool name -> capability -> declared binding
}

func NewRegistry() *Registry {
	return &Registry{active: make(map[string]Activation), bindings: make(map[string]map[string]ToolBinding)}
}

func Decode(data []byte) (Manifest, error) {
	var manifest Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode capability manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Load(data []byte) (Manifest, error) { return Decode(data) }

func (m Manifest) Validate() error {
	if m.Name == "" {
		return errors.New("capability name is required")
	}
	if m.Version == "" {
		return errors.New("capability version is required")
	}
	seen := make(map[string]bool, len(m.ToolBindings))
	for _, binding := range m.ToolBindings {
		if binding.Name == "" {
			return fmt.Errorf("capability %s has a tool binding without a name", m.Name)
		}
		if seen[binding.Name] {
			return fmt.Errorf("capability %s binds tool %s more than once", m.Name, binding.Name)
		}
		seen[binding.Name] = true
		if binding.ModelVisibility == "" || binding.Executor == "" {
			return fmt.Errorf("capability %s tool %s must declare model_visibility and executor", m.Name, binding.Name)
		}
		tb := tools.Binding{Name: binding.Name, ModelVisibility: binding.ModelVisibility, Executor: binding.Executor}
		if err := tb.Validate(); err != nil {
			return fmt.Errorf("capability %s tool %s: %w", m.Name, binding.Name, err)
		}
	}
	return nil
}

func (r *Registry) Activate(manifest Manifest, source string, clientCapabilities []string) (Activation, error) {
	if err := manifest.Validate(); err != nil {
		return Activation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.active[manifest.Name]; ok {
		if existing.Manifest.Version == manifest.Version && existing.Source == source {
			return existing, nil
		}
		return Activation{}, fmt.Errorf("capability %s is already active at version %s", manifest.Name, existing.Manifest.Version)
	}
	for _, binding := range manifest.ToolBindings {
		if r.bindings[binding.Name] == nil {
			r.bindings[binding.Name] = make(map[string]ToolBinding)
		}
		r.bindings[binding.Name][manifest.Name] = binding
	}
	missing := missingCapabilities(manifest.ClientCapabilities, clientCapabilities)
	activation := Activation{Manifest: manifest, Source: source, MissingClientCapabilities: missing}
	r.active[manifest.Name] = activation
	return activation, nil
}

func (r *Registry) Active() []Activation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.active))
	for name := range r.active {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Activation, 0, len(names))
	for _, name := range names {
		out = append(out, r.active[name])
	}
	return out
}

// Resolve returns the one capability that owns a tool for this invocation
// context. An empty context selects a binding only when exactly one active
// capability declares the tool; ambiguity fails closed.
func (r *Registry) Resolve(toolName, capabilityContext string) (string, bool) {
	name, _, ok := r.ResolveBinding(toolName, capabilityContext)
	return name, ok
}

func (r *Registry) ResolveBinding(toolName, capabilityContext string) (string, ToolBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	owners := r.bindings[toolName]
	if capabilityContext != "" {
		binding, ok := owners[capabilityContext]
		return capabilityContext, binding, ok
	}
	if len(owners) != 1 {
		return "", ToolBinding{}, false
	}
	for name, binding := range owners {
		return name, binding, true
	}
	return "", ToolBinding{}, false
}

func (r *Registry) Apply(toolName, capabilityContext string, binding tools.Binding) tools.Binding {
	if name, declared, ok := r.ResolveBinding(toolName, capabilityContext); ok {
		binding.Capability = name
		binding.ModelVisibility = declared.ModelVisibility
		binding.Executor = declared.Executor
	}
	return binding
}

func missingCapabilities(required, available []string) []string {
	have := make(map[string]bool, len(available))
	for _, capability := range available {
		have[capability] = true
	}
	var missing []string
	seen := make(map[string]bool)
	for _, capability := range required {
		if capability != "" && !have[capability] && !seen[capability] {
			seen[capability] = true
			missing = append(missing, capability)
		}
	}
	sort.Strings(missing)
	return missing
}
