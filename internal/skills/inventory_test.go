package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectScopedReportsScopesShadowsAndResources(t *testing.T) {
	skillsRoot := t.TempDir()
	workspace := t.TempDir()
	systemDir := filepath.Join(skillsRoot, ".system")
	userDir := skillsRoot
	projectDir := filepath.Join(workspace, "skills")
	agentsDir := filepath.Join(workspace, ".agents", "skills")

	writeSkill(t, systemDir, "shared", "---\nname: shared\ndescription: system\n---\nsystem")
	writeSkill(t, userDir, "shared", "---\nname: shared\ndescription: user\n---\nuser")
	writeSkill(t, systemDir, "system-only", "---\nname: system-only\ndescription: system only\nversion: 3\n---\nsystem")
	writeSkill(t, userDir, "user-only", "---\nname: user-only\ndescription: user only\n---\nuser")
	writeSkill(t, projectDir, "shared", "---\nname: shared\ndescription: project\n---\nproject")
	writeSkill(t, agentsDir, "shared", "---\nname: shared\ndescription: agents\n---\nagents")
	writeSkill(t, agentsDir, "project-only", "---\nname: project-only\ndescription: project only\n---\nproject")
	if err := os.MkdirAll(filepath.Join(agentsDir, "shared", "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "shared", "references", "guide.md"), []byte("guide"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := InspectScoped(WorkspaceLoadOptions(skillsRoot, workspace))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 {
		t.Fatalf("entries = %d, want 7: %#v", len(entries), entries)
	}

	var active, shadowed []InventoryEntry
	for _, entry := range entries {
		if entry.Name == "shared" {
			if entry.Enabled {
				active = append(active, entry)
			} else {
				shadowed = append(shadowed, entry)
			}
		}
	}
	if len(active) != 1 || active[0].Scope != ScopeProject || active[0].Description != "agents" {
		t.Fatalf("active shared = %#v", active)
	}
	if active[0].Resources == nil || active[0].Resources[0].Kind != "reference" {
		t.Fatalf("resources = %#v", active[0].Resources)
	}
	if len(active[0].Shadows) != 3 {
		t.Fatalf("shadows = %#v, want project, user, system chain", active[0].Shadows)
	}
	if len(shadowed) != 3 {
		t.Fatalf("shadowed = %#v", shadowed)
	}
	for _, entry := range shadowed {
		if entry.ShadowedBy == nil || entry.ShadowedBy.Name != "shared" {
			t.Fatalf("shadowed entry lacks shadowed_by: %#v", entry)
		}
	}

	byName := make(map[string]InventoryEntry)
	for _, entry := range entries {
		if entry.Enabled {
			byName[entry.Name] = entry
		}
	}
	if byName["system-only"].Scope != ScopeSystem || byName["user-only"].Scope != ScopeUser || byName["project-only"].Scope != ScopeProject {
		t.Fatalf("scope classification = %#v", byName)
	}
}

func TestInspectScopedPluginNamespaceAndMalformed(t *testing.T) {
	userDir := t.TempDir()
	projectDir := t.TempDir()
	pluginDir := t.TempDir()
	writeSkill(t, userDir, "plain", "---\nname: plain\ndescription: plain\n---\nbody")
	writeSkill(t, pluginDir, "search", "---\nname: search\ndescription: search\n---\nbody")
	if err := os.WriteFile(filepath.Join(projectDir, "broken.md"), []byte("not frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := InspectScoped(LoadOptions{
		UserDir: userDir, ProjectDirs: []string{projectDir},
		Plugins: []Root{{Dir: pluginDir, PluginName: "docs"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var plugin, invalid *InventoryEntry
	for i := range entries {
		switch entries[i].Name {
		case "docs:search":
			plugin = &entries[i]
		case "broken":
			invalid = &entries[i]
		}
	}
	if plugin == nil || plugin.Scope != ScopePlugin || plugin.PluginName != "docs" || plugin.Source != "plugin:docs" || !plugin.Enabled {
		t.Fatalf("plugin entry = %#v", plugin)
	}
	if invalid == nil || invalid.Status != InventoryStatusInvalid || invalid.Enabled || len(invalid.ParseErrors) == 0 || invalid.ContentHash == "" {
		t.Fatalf("invalid entry = %#v", invalid)
	}
}
