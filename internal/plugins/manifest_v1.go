package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const PluginSchemaV1 = "codeagent.plugin.v1"

var safeComponent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Manifest is the CodeAgent plugin manifest. Capability declarations are kept
// as raw JSON so the installer can digest and preserve fields that this phase
// does not execute yet (apps/hooks/interface).
type Manifest struct {
	Schema      string          `json:"schema"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Skills      ManifestSkills  `json:"skills"`
	MCPServers  json.RawMessage `json:"mcpServers,omitempty"`
	Apps        json.RawMessage `json:"apps,omitempty"`
	Hooks       json.RawMessage `json:"hooks,omitempty"`
	Interface   json.RawMessage `json:"interface,omitempty"`
}

// ManifestSkills accepts both Codex's single path and CodeAgent's explicit list.
type ManifestSkills []string

func (s *ManifestSkills) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		*s = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(trimmed, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var list []string
	if err := json.Unmarshal(trimmed, &list); err == nil {
		*s = list
		return nil
	}
	return fmt.Errorf("skills must be a path string or an array of path strings")
}

func LoadManifest(pluginRoot string) (*Manifest, string, error) {
	manifestPath, err := FindManifest(pluginRoot)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, "", fmt.Errorf("read plugin manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, "", fmt.Errorf("parse plugin manifest: %w", err)
	}
	if err := ValidateManifest(pluginRoot, &manifest); err != nil {
		return nil, "", err
	}
	return &manifest, manifestPath, nil
}

func FindManifest(pluginRoot string) (string, error) {
	for _, rel := range []string{
		filepath.Join(".codeagent-plugin", "plugin.json"),
		"plugin.json",
		filepath.Join(".claude-plugin", "plugin.json"),
	} {
		candidate := filepath.Join(pluginRoot, rel)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("plugin manifest not found (expected .codeagent-plugin/plugin.json)")
}

func ValidateManifest(pluginRoot string, manifest *Manifest) error {
	if manifest.Schema != PluginSchemaV1 {
		return fmt.Errorf("unsupported plugin schema %q", manifest.Schema)
	}
	if !safeComponent.MatchString(manifest.Name) {
		return fmt.Errorf("invalid plugin name %q", manifest.Name)
	}
	if !safeComponent.MatchString(manifest.Version) {
		return fmt.Errorf("invalid plugin version %q", manifest.Version)
	}
	if strings.TrimSpace(manifest.Description) == "" {
		return fmt.Errorf("plugin description is required")
	}
	for _, declared := range manifest.Skills {
		resolved, err := ResolvePluginPath(pluginRoot, declared)
		if err != nil {
			return fmt.Errorf("plugin skill path %q: %w", declared, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("plugin skill path %q: %w", declared, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("plugin skill path %q is not a directory", declared)
		}
	}
	if len(manifest.MCPServers) > 0 && !bytes.Equal(bytes.TrimSpace(manifest.MCPServers), []byte("null")) {
		var path string
		if err := json.Unmarshal(manifest.MCPServers, &path); err == nil {
			resolved, err := ResolvePluginPath(pluginRoot, path)
			if err != nil {
				return fmt.Errorf("plugin mcpServers path: %w", err)
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return fmt.Errorf("plugin mcpServers path: %w", err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("plugin mcpServers path is not a file")
			}
		} else {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(manifest.MCPServers, &object); err != nil {
				return fmt.Errorf("plugin mcpServers must be a path or object: %w", err)
			}
		}
	}
	return nil
}

// ResolvePluginPath resolves a manifest-relative path and rejects escapes.
func ResolvePluginPath(pluginRoot, relative string) (string, error) {
	cleaned := strings.TrimPrefix(strings.TrimSpace(relative), "./")
	if cleaned == "" || strings.Contains(cleaned, "\\") || filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("unsafe relative path")
	}
	clean := filepath.Clean(cleaned)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes plugin root")
	}
	resolved := filepath.Join(pluginRoot, clean)
	rootAbs, err := filepath.Abs(pluginRoot)
	if err != nil {
		return "", err
	}
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if resolvedAbs != rootAbs && !strings.HasPrefix(resolvedAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes plugin root")
	}
	return resolvedAbs, nil
}
