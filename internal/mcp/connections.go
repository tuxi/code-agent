package mcp

import (
	"sort"
	"time"

	"code-agent/internal/tools"
)

// ConnectionSnapshot contains public, non-secret connection evidence. It does
// not contain transport endpoints, command arguments, headers or environment.
type ConnectionSnapshot struct {
	Name                string           `json:"name"`
	Status              string           `json:"status"`
	ObservedAt          string           `json:"observed_at"`
	DiscoveredToolCount *int             `json:"discovered_tool_count"`
	Tools               []ConnectionTool `json:"tools"`
}
type ConnectionTool struct {
	implementation tools.Tool
	Name           string `json:"name"`
	Description    string `json:"description"`
}

func (m *Manager) Connections() []ConnectionSnapshot {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	out := make([]ConnectionSnapshot, 0, len(m.connections))
	for _, entry := range m.connections {
		entry.Tools = append([]ConnectionTool{}, entry.Tools...)
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) recordConnection(name, status string, discovered []tools.Tool) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.connections == nil {
		m.connections = map[string]ConnectionSnapshot{}
	}
	entry := m.connections[name]
	entry.Name = name
	entry.Status = status
	entry.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if status == "connected" {
		n := len(discovered)
		entry.DiscoveredToolCount = &n
		entry.Tools = []ConnectionTool{}
		for _, tool := range discovered {
			entry.Tools = append(entry.Tools, ConnectionTool{implementation: tool, Name: tool.Name(), Description: tool.Description()})
		}
	}
	m.connections[name] = entry
}

// WithSource annotates a configuration layer without changing its credentials
// or whole-entry override semantics. The annotation is excluded from JSON.
func WithSource(config Config, source string) Config {
	out := Config{Servers: append([]ServerConfig(nil), config.Servers...)}
	for i := range out.Servers {
		out.Servers[i].Source = source
	}
	return out
}

func (t ConnectionTool) RegisteredIn(registry *tools.Registry) bool {
	if registry == nil {
		return false
	}
	registered, ok := registry.Get(t.Name)
	return ok && registered == t.implementation
}
