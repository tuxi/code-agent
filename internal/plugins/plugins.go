// Package plugins implements the plugin marketplace system (Phase 6.1 P2.1).
// It parses .claude-plugin/marketplace.json, installs skill plugins from Git
// repositories, and manages the symlink farm under ~/.codeagent/skills/.
//
// Skills are installed with a prefix ("<marketplace-name>/<plugin-name>/") in
// the skills directory so the user can tell which marketplace a skill came from,
// and so two plugins from different marketplaces with the same skill name don't
// collide silently.
package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Marketplace is the top-level manifest in .claude-plugin/marketplace.json.
type Marketplace struct {
	Name     string   `json:"name"`
	Owner    Owner    `json:"owner"`
	Metadata Metadata `json:"metadata"`
	Plugins  []Plugin `json:"plugins"`
}

// Owner identifies the marketplace maintainer.
type Owner struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Metadata carries version and description for the marketplace itself.
type Metadata struct {
	Description string `json:"description"`
	Version     string `json:"version"`
}

// Plugin is a named bundle of skills inside a marketplace.
type Plugin struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Source      string   `json:"source"`  // relative path to the marketplace root
	Strict      bool     `json:"strict"`  // false = skills can be individually used
	Skills      []string `json:"skills"`  // relative paths to skill directories
}

// InstalledPlugin records that a plugin was installed from a marketplace.
type InstalledPlugin struct {
	MarketplaceName string
	MarketplacePath string // absolute path to the cloned repo
	Plugin          Plugin
}

// ParseMarketplace reads a marketplace.json file and returns the parsed manifest.
func ParseMarketplace(path string) (*Marketplace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read marketplace.json: %w", err)
	}
	var m Marketplace
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse marketplace.json: %w", err)
	}
	if m.Name == "" {
		return nil, fmt.Errorf("marketplace.json missing required field 'name'")
	}
	return &m, nil
}

// SkillsDir returns the user-level skills directory (~/.codeagent/skills/).
func SkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return filepath.Join(home, ".codeagent", "skills"), nil
}

// PluginsDir returns the directory where cloned marketplaces live
// (~/.codeagent/plugins/).
func PluginsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return filepath.Join(home, ".codeagent", "plugins"), nil
}

// InstalledManifestPath returns the path to the installed-plugins manifest
// (~/.codeagent/plugins/installed.json).
func InstalledManifestPath() (string, error) {
	dir, err := PluginsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "installed.json"), nil
}

// LoadInstalled reads the installed-plugins manifest.
func LoadInstalled() ([]InstalledPlugin, error) {
	path, err := InstalledManifestPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var installed []InstalledPlugin
	if err := json.Unmarshal(data, &installed); err != nil {
		return nil, fmt.Errorf("parse installed.json: %w", err)
	}
	return installed, nil
}

// SaveInstalled writes the installed-plugins manifest.
func SaveInstalled(installed []InstalledPlugin) error {
	path, err := InstalledManifestPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
