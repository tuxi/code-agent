package runtime

import (
	"path/filepath"
	"sort"

	"code-agent/internal/mcp"
	"code-agent/internal/plugins"
	"code-agent/internal/tools"
)

type ConnectorTool struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Registered     bool   `json:"registered"`
	CatalogVisible *bool  `json:"catalog_visible"`
}
type Connector struct {
	ExposureSnapshotID      string          `json:"exposure_snapshot_id,omitempty"`
	ID                      string          `json:"connector_id"`
	Kind                    string          `json:"kind"`
	Name                    string          `json:"display_name"`
	Source                  string          `json:"source"`
	Status                  string          `json:"status"`
	Reason                  string          `json:"reason,omitempty"`
	ObservedAt              string          `json:"last_checked_at,omitempty"`
	Health                  string          `json:"health"`
	DiscoveredToolCount     *int            `json:"discovered_tool_count"`
	RegisteredToolCount     *int            `json:"registered_tool_count"`
	AvailableToolCount      *int            `json:"available_tool_count"`
	CatalogVisibleToolCount *int            `json:"catalog_visible_tool_count"`
	ExposedToolCount        *int            `json:"exposed_tool_count"`
	Tools                   []ConnectorTool `json:"tools"`
}
type ConnectorSnapshot struct {
	State                   string      `json:"state"`
	Diagnostics             []string    `json:"diagnostics"`
	Items                   []Connector `json:"items"`
	ClientCapabilitiesState string      `json:"client_capabilities_state"`
}

func resolveWorkspaceMCP(mc *workspaceMCP, root string) ([]mcp.ServerConfig, []string) {
	diagnostics := []string{}
	resolved, err := mcp.ResolveDesktop(root, mc.inheritClaude)
	if err != nil {
		resolved = mcp.Config{}
		diagnostics = append(diagnostics, "workspace_mcp_config_invalid")
	}
	pluginConfig := mcp.Config{}
	if mc.cfg.GlobalSkillsDir != "" {
		pluginConfig, err = plugins.MCPConfigForSkillsDir(mc.cfg.GlobalSkillsDir)
		if err != nil {
			pluginConfig = mcp.Config{}
			diagnostics = append(diagnostics, "plugin_mcp_config_invalid")
		}
	}
	return mcp.Merge(mcp.WithSource(pluginConfig, "plugin"), mcp.WithSource(mcp.Config{Servers: mc.injected}, "host"), resolved).Servers, diagnostics
}

// ConnectorSnapshot never calls Get: observing a cold workspace must not open
// its store, load skills or start MCP processes. Reloads return loading rather
// than blocking the UI behind a possibly slow handshake.
func (wr *WorkspaceRegistry) ConnectorSnapshot(root string) ConnectorSnapshot {
	out := ConnectorSnapshot{State: "ready", Diagnostics: []string{}, Items: []Connector{}, ClientCapabilitiesState: "unavailable"}
	abs, err := filepath.Abs(root)
	if err != nil || root == "" {
		out.State = "unavailable"
		return out
	}
	if !wr.mu.TryLock() {
		out.State = "loading"
		return out
	}
	inst, mc := wr.instances[abs], wr.mcp
	wr.mu.Unlock()
	var configs []mcp.ServerConfig
	var registry *tools.Registry
	var connections []mcp.ConnectionSnapshot
	initialized := false
	connecting := false
	if inst != nil {
		if !inst.mu.TryRLock() {
			out.State = "loading"
			return out
		}
		configs = append(configs, inst.mcpInventory...)
		out.Diagnostics = append(out.Diagnostics, inst.mcpDiagnostics...)
		registry = inst.ToolReg
		initialized = inst.mcpInitialized
		connecting = !initialized && inst.mcpCfg != nil
		if inst.MCPMgr != nil {
			connections = inst.MCPMgr.Connections()
		}
		inst.mu.RUnlock()
	}
	if (inst == nil || (!initialized && len(configs) == 0)) && mc != nil {
		configs, out.Diagnostics = resolveWorkspaceMCP(mc, abs)
	}
	if mc == nil {
		out.State = "unavailable"
		out.Diagnostics = append(out.Diagnostics, "mcp_not_configured")
		return out
	}
	byName := map[string]mcp.ConnectionSnapshot{}
	for _, c := range connections {
		byName[c.Name] = c
	}
	for _, cfg := range configs {
		row := Connector{ID: "mcp:" + cfg.Name, Kind: "mcp", Name: cfg.Name, Source: cfg.Source, Status: "unknown", Health: "not_probed", Tools: []ConnectorTool{}}
		if cfg.Type == mcp.TransportStdio && !mc.cfg.Profile.AllowsSubprocess() {
			row.Status = "disabled"
			row.Reason = "transport_not_supported_by_profile"
		}
		if connection, ok := byName[cfg.Name]; ok {
			row.Status = connection.Status
			row.ObservedAt = connection.ObservedAt
			registered, visible, available := 0, 0, 0
			row.DiscoveredToolCount = connection.DiscoveredToolCount
			for _, tool := range connection.Tools {
				hidden := false
				item := ConnectorTool{Name: tool.Name, Description: tool.Description, CatalogVisible: &hidden}
				if registry != nil {
					if tool.RegisteredIn(registry) {
						item.Registered = true
						registered++
						if b, ok := registry.Binding(tool.Name); ok && b.ModelVisibility == tools.VisibilityDirect {
							visible++
							visibleValue := true
							item.CatalogVisible = &visibleValue
						}
					}
				}
				row.Tools = append(row.Tools, item)
			}
			if row.Status == "connected" {
				available = registered
			}
			row.RegisteredToolCount = &registered
			row.CatalogVisibleToolCount = &visible
			row.AvailableToolCount = &available
			if row.Status == "failed" {
				row.Reason = "connection_failed"
			}
		} else if row.Status != "disabled" {
			if connecting {
				row.Status = "connecting"
				row.Reason = "workspace_connecting"
			} else if initialized {
				row.Reason = "connection_not_available"
			} else {
				row.Reason = "workspace_not_connected"
			}
		}
		out.Items = append(out.Items, row)
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID < out.Items[j].ID })
	if len(out.Diagnostics) > 0 {
		out.State = "stale"
	} else if len(out.Items) == 0 {
		out.State = "empty"
		if connecting {
			out.State = "loading"
		}
	}
	return out
}
