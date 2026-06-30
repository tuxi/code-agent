package plugins

import (
	"fmt"
	"os"
	"path/filepath"
)

// skillName returns the prefix-qualified name for a skill: "<marketplace>/<plugin>/<skill-dir-name>".
// This prevents collisions between marketplaces and makes it obvious where a skill came from.
func skillName(marketplace, plugin, skillPath string) string {
	base := filepath.Base(skillPath)
	return fmt.Sprintf("%s/%s/%s", marketplace, plugin, base)
}

// InstallPlugin clones a marketplace repo (if not already present), reads its
// marketplace.json, finds the named plugin, and symlinks each of its skills into
// ~/.codeagent/skills/. Returns the number of skills installed.
//
// If pluginName is empty, returns an error listing the available plugins.
func InstallPlugin(repoURL, pluginName string) (count int, err error) {
	pluginsDir, err := PluginsDir()
	if err != nil {
		return 0, err
	}
	skillsDir, err := SkillsDir()
	if err != nil {
		return 0, err
	}

	// Clone the repo if not already present.
	marketplacePath, err := cloneRepo(repoURL, pluginsDir)
	if err != nil {
		return 0, err
	}

	// Parse the marketplace manifest.
	manifestPath := filepath.Join(marketplacePath, ".claude-plugin", "marketplace.json")
	m, err := ParseMarketplace(manifestPath)
	if err != nil {
		return 0, err
	}

	// If no plugin specified, list the available ones.
	if pluginName == "" {
		return 0, fmt.Errorf("no plugin specified; available plugins in %q:\n%s", m.Name, listPlugins(m))
	}

	// Find the requested plugin.
	var target *Plugin
	for i := range m.Plugins {
		if m.Plugins[i].Name == pluginName {
			target = &m.Plugins[i]
			break
		}
	}
	if target == nil {
		return 0, fmt.Errorf("plugin %q not found in marketplace %q; available:\n%s", pluginName, m.Name, listPlugins(m))
	}

	// Symlink each skill into the skills directory.
	for _, skillRel := range target.Skills {
		src := filepath.Join(marketplacePath, target.Source, skillRel)
		dst := filepath.Join(skillsDir, skillName(m.Name, target.Name, skillRel))

		// Ensure the parent directory exists.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return count, fmt.Errorf("create skill dir: %w", err)
		}
		// Remove existing symlink/junk if present.
		_ = os.Remove(dst)
		if err := os.Symlink(src, dst); err != nil {
			return count, fmt.Errorf("symlink %s → %s: %w", dst, src, err)
		}
		count++
	}

	// Record the installation.
	installed, err := LoadInstalled()
	if err != nil {
		return count, fmt.Errorf("load installed manifest: %w", err)
	}
	installed = append(installed, InstalledPlugin{
		MarketplaceName: m.Name,
		MarketplacePath: marketplacePath,
		Plugin:          *target,
	})
	if err := SaveInstalled(installed); err != nil {
		return count, fmt.Errorf("save installed manifest: %w", err)
	}

	return count, nil
}

// UninstallPlugin removes all symlinks for a plugin from ~/.codeagent/skills/ and
// updates the installed manifest.
func UninstallPlugin(pluginName string) error {
	installed, err := LoadInstalled()
	if err != nil {
		return err
	}
	skillsDir, err := SkillsDir()
	if err != nil {
		return err
	}

	var remaining []InstalledPlugin
	found := false
	for _, ip := range installed {
		if ip.Plugin.Name == pluginName {
			found = true
			for _, skillRel := range ip.Plugin.Skills {
				name := skillName(ip.MarketplaceName, ip.Plugin.Name, skillRel)
				dst := filepath.Join(skillsDir, name)
				if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove symlink %s: %w", dst, err)
				}
				// Also clean up empty parent dirs.
				_ = os.Remove(filepath.Dir(dst))
			}
		} else {
			remaining = append(remaining, ip)
		}
	}
	if !found {
		return fmt.Errorf("plugin %q is not installed", pluginName)
	}
	return SaveInstalled(remaining)
}

func listPlugins(m *Marketplace) string {
	var s string
	for _, p := range m.Plugins {
		s += fmt.Sprintf("  %s — %s (%d skills)\n", p.Name, p.Description, len(p.Skills))
	}
	return s
}
