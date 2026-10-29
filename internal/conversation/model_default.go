package conversation

import "code-agent/internal/settings"

// DefaultModelReference reads the builder's current default without changing a
// session. The bool distinguishes an unsupported legacy builder from a runtime
// that supports resolution but currently has no model configured.
func (e *TurnExecutor) DefaultModelReference() (*settings.ModelConfig, bool) {
	if e == nil {
		return nil, false
	}
	resolver, ok := e.rb.(ModelResolver)
	if !ok {
		return nil, false
	}
	mc, err := resolver.ResolveModel("")
	if err != nil {
		return nil, true
	}
	return mc, true
}
