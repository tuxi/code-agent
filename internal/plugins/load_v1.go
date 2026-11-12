package plugins

import (
	"encoding/json"
	"fmt"
	"os"

	"code-agent/internal/mcp"
	"code-agent/internal/skills"
)

// VerifyInstalled checks the immutable cache digest and manifest digest before a
// plugin capability is exposed.
func VerifyInstalled(entry InstalledPluginV1) (*Manifest, error) {
	manifest, manifestPath, err := LoadManifest(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	if manifest.Name != entry.Name || manifest.Version != entry.Version {
		return nil, fmt.Errorf("plugin cache identity mismatch for %q@%q", entry.Name, entry.Version)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	if got := hashBytes(data); got != entry.ManifestSHA256 {
		return nil, fmt.Errorf("plugin %q manifest digest mismatch", entry.Name)
	}
	digest, err := digestTree(entry.Path)
	if err != nil {
		return nil, err
	}
	if digest != entry.BundleSHA256 {
		return nil, fmt.Errorf("plugin %q bundle digest mismatch", entry.Name)
	}
	return manifest, nil
}

// SkillRootsForSkillsDir returns only enabled, verified plugin skill roots.
func SkillRootsForSkillsDir(skillsDir string) ([]skills.Root, error) {
	registry, err := LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil {
		return nil, err
	}
	var roots []skills.Root
	for _, entry := range registry.Plugins {
		if !entry.Enabled {
			continue
		}
		manifest, err := VerifyInstalled(entry)
		if err != nil {
			return nil, err
		}
		for _, declared := range manifest.Skills {
			root, err := ResolvePluginPath(entry.Path, declared)
			if err != nil {
				return nil, fmt.Errorf("plugin %q: %w", entry.Name, err)
			}
			roots = append(roots, skills.Root{Dir: root, Scope: skills.ScopePlugin, PluginName: entry.Name})
		}
	}
	return roots, nil
}

// MCPConfigForSkillsDir loads MCP declarations from enabled plugins. Server names
// are namespaced as <plugin>__<server>; disabled plugins contribute nothing.
func MCPConfigForSkillsDir(skillsDir string) (mcp.Config, error) {
	registry, err := LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil {
		return mcp.Config{}, err
	}
	var layers []mcp.Config
	for _, entry := range registry.Plugins {
		if !entry.Enabled {
			continue
		}
		manifest, err := VerifyInstalled(entry)
		if err != nil {
			return mcp.Config{}, err
		}
		if len(manifest.MCPServers) == 0 {
			continue
		}
		var path string
		var cfg mcp.Config
		if err := json.Unmarshal(manifest.MCPServers, &path); err == nil {
			resolved, err := ResolvePluginPath(entry.Path, path)
			if err != nil {
				return mcp.Config{}, err
			}
			cfg, err = mcp.LoadFile(resolved)
			if err != nil {
				return mcp.Config{}, fmt.Errorf("plugin %q mcpServers: %w", entry.Name, err)
			}
		} else {
			wrapped, err := json.Marshal(struct {
				MCPServers json.RawMessage `json:"mcpServers"`
			}{MCPServers: manifest.MCPServers})
			if err != nil {
				return mcp.Config{}, err
			}
			cfg, err = mcp.ParseJSON(wrapped)
			if err != nil {
				return mcp.Config{}, fmt.Errorf("plugin %q mcpServers: %w", entry.Name, err)
			}
		}
		for i := range cfg.Servers {
			cfg.Servers[i].Name = entry.Name + "__" + cfg.Servers[i].Name
		}
		layers = append(layers, cfg)
	}
	return mcp.Merge(layers...), nil
}
