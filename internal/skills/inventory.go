package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Inventory status values describe a discovered skill's effective state.
const (
	InventoryStatusActive   = "active"
	InventoryStatusShadowed = "shadowed"
	InventoryStatusInvalid  = "invalid"
)

// SkillOrigin is the non-content identity of a discovered skill. Inventory
// callers can use it to explain precedence without reading the skill body.
type SkillOrigin struct {
	Name       string
	Scope      Scope
	Source     string
	PluginName string
	Path       string
}

// InventoryEntry is one discovered skill, including candidates that are
// invalid or shadowed. Enabled is true only for the effective active entry.
type InventoryEntry struct {
	Name        string
	Label       string
	Scope       Scope
	Source      string
	PluginName  string
	Path        string
	Dir         string
	DisplayName string
	Version     string
	Description string
	License     string
	Interface   Interface
	ContentHash string
	Enabled     bool
	Status      string
	Resources   []Resource
	ParseErrors []string
	Warnings    []string
	ShadowedBy  *SkillOrigin
	Shadows     []SkillOrigin
}

// InspectScoped discovers every skill candidate under the same roots and
// precedence rules as LoadScoped. It does not change the runtime registry.
func InspectScoped(opts LoadOptions) ([]InventoryEntry, error) {
	roots := scopedRoots(opts)
	entries := make([]InventoryEntry, 0)
	active := make(map[string]int)

	for _, root := range roots {
		discovered, err := discoverRoot(root)
		if err != nil {
			return nil, err
		}
		for _, item := range discovered {
			entry := InventoryEntry{
				Name:        item.skill.Meta.Name,
				Label:       item.label,
				Scope:       root.Scope,
				Source:      root.Source,
				PluginName:  root.PluginName,
				Path:        item.skill.Path,
				Dir:         item.skill.Dir,
				DisplayName: item.skill.Meta.DisplayName,
				Version:     item.skill.Meta.Version,
				Description: item.skill.Meta.Description,
				License:     item.skill.Meta.License,
				Interface:   item.skill.Interface,
				ContentHash: item.skill.ContentHash,
				Resources:   append([]Resource(nil), item.skill.Resources...),
			}
			if item.parseErr != "" {
				entry.Name = item.label
				entry.Enabled = false
				entry.Status = InventoryStatusInvalid
				entry.ParseErrors = []string{item.parseErr}
				entries = append(entries, entry)
				continue
			}
			entry.Enabled = true
			entry.Status = InventoryStatusActive
			origin := item.origin()

			existingIndex, exists := active[entry.Name]
			if !exists {
				active[entry.Name] = len(entries)
				entries = append(entries, entry)
				continue
			}

			existing := &entries[existingIndex]
			if root.Scope == ScopePlugin && existing.Scope != ScopePlugin {
				entry.Enabled = false
				entry.Status = InventoryStatusShadowed
				entry.ShadowedBy = &SkillOrigin{
					Name: existing.Name, Scope: existing.Scope, Source: existing.Source,
					PluginName: existing.PluginName, Path: existing.Path,
				}
				entries = append(entries, entry)
				continue
			}
			if scopeRank(root.Scope) < scopeRank(existing.Scope) {
				entry.Enabled = false
				entry.Status = InventoryStatusShadowed
				entry.ShadowedBy = &SkillOrigin{
					Name: existing.Name, Scope: existing.Scope, Source: existing.Source,
					PluginName: existing.PluginName, Path: existing.Path,
				}
				entries = append(entries, entry)
				continue
			}

			// The new candidate wins. Preserve the full lower-precedence chain on
			// the effective entry so management UIs can explain every shadow.
			entry.Shadows = append(entry.Shadows, SkillOrigin{
				Name: existing.Name, Scope: existing.Scope, Source: existing.Source,
				PluginName: existing.PluginName, Path: existing.Path,
			})
			entry.Shadows = append(entry.Shadows, existing.Shadows...)
			existing.Enabled = false
			existing.Status = InventoryStatusShadowed
			existing.ShadowedBy = &origin
			entries = append(entries, entry)
			active[entry.Name] = len(entries) - 1
		}
	}

	for i := range entries {
		entries[i].Warnings = inventoryWarnings(&entries[i])
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		if entries[i].Scope != entries[j].Scope {
			return scopeRank(entries[i].Scope) < scopeRank(entries[j].Scope)
		}
		return entries[i].Path < entries[j].Path
	})
	return entries, nil
}

func inventoryWarnings(entry *InventoryEntry) []string {
	var warnings []string
	switch entry.Status {
	case InventoryStatusInvalid:
		warnings = append(warnings, fmt.Sprintf("skill %q could not be parsed", entry.Name))
	case InventoryStatusShadowed:
		if entry.ShadowedBy != nil {
			warnings = append(warnings, fmt.Sprintf("skill %q is shadowed by %s skill %q", entry.Name, entry.ShadowedBy.Scope, entry.ShadowedBy.Name))
		}
	}
	if len(entry.Shadows) > 0 {
		warnings = append(warnings, fmt.Sprintf("skill %q shadows %d lower-precedence candidate(s)", entry.Name, len(entry.Shadows)))
	}
	return warnings
}

type discoveredSkill struct {
	label    string
	skill    Skill
	parseErr string
}

func (d discoveredSkill) origin() SkillOrigin {
	return SkillOrigin{
		Name: d.skill.Meta.Name, Scope: d.skill.Scope, Source: d.skill.Source,
		PluginName: pluginNameForSource(d.skill.Source), Path: d.skill.Path,
	}
}

func pluginNameForSource(source string) string {
	const prefix = "plugin:"
	if strings.HasPrefix(source, prefix) {
		return strings.TrimPrefix(source, prefix)
	}
	return ""
}

// discoverRoot is shared by the runtime loader and inventory inspection so both
// apply the same path/frontmatter rules.
func discoverRoot(root Root) ([]discoveredSkill, error) {
	entries, err := os.ReadDir(root.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []discoveredSkill
	for _, entry := range entries {
		if entry.Name() == ".system" && root.Scope == ScopeUser {
			continue
		}
		var content []byte
		var label string
		var skillDir string
		var srcPath string
		var modTime time.Time

		if entry.IsDir() {
			p := filepath.Join(root.Dir, entry.Name(), "SKILL.md")
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			body, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			content = body
			label = entry.Name()
			skillDir = filepath.Join(root.Dir, entry.Name())
			srcPath = p
			modTime = info.ModTime()
		} else if strings.HasSuffix(entry.Name(), ".md") {
			p := filepath.Join(root.Dir, entry.Name())
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			body, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			content = body
			label = strings.TrimSuffix(entry.Name(), ".md")
			srcPath = p
			modTime = info.ModTime()
		} else {
			continue
		}

		sum := sha256.Sum256(content)
		skill := Skill{
			Scope: root.Scope, Source: root.Source, Path: srcPath,
			ContentHash: hex.EncodeToString(sum[:]), Dir: skillDir,
		}
		parsed, parseErr := parseSkill(string(content))
		parseError := ""
		if parseErr == nil {
			if root.PluginName != "" {
				parsed.namespace = root.PluginName
				parsed.Meta.Name = root.PluginName + ":" + parsed.Meta.Name
			}
			parsed.Dir = skillDir
			parsed.Scope = root.Scope
			parsed.Source = root.Source
			parsed.Path = srcPath
			parsed.ContentHash = skill.ContentHash
			parsed.Enabled = true
			parsed.sourcePath = srcPath
			parsed.modTime = modTime
			if skillDir != "" {
				parsed.Resources = scanResources(skillDir)
			}
			skill = parsed
		} else {
			parseError = parseErr.Error()
		}
		out = append(out, discoveredSkill{label: label, skill: skill, parseErr: parseError})
	}
	return out, nil
}
