package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/plugins"
)

func TestSkillPluginManagerInventoryScopesAndWorkspaceIsolation(t *testing.T) {
	dataRoot := t.TempDir()
	skillsDir := filepath.Join(dataRoot, "skills")
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	writeManagementSkill(t, filepath.Join(skillsDir, ".system"), "system-skill")
	writeManagementSkill(t, skillsDir, "user-skill")
	writeManagementSkill(t, filepath.Join(workspaceA, ".agents", "skills"), "project-a")
	writeManagementSkill(t, filepath.Join(workspaceB, "skills"), "project-b")

	manager := NewSkillPluginManager(skillsDir)
	inventory, err := manager.SkillInventory(context.Background(), "sess_a", workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	if !inventory.Scope.Resolved || inventory.Scope.SessionID != "sess_a" || inventory.Scope.DisplayName == "" {
		t.Fatalf("scope = %#v", inventory.Scope)
	}
	byName := make(map[string]SkillInventoryItem)
	for _, item := range inventory.Items {
		byName[item.Name] = item
		if strings.Contains(item.Path, dataRoot) || strings.Contains(item.Path, workspaceA) {
			t.Fatalf("path leaked absolute root: %#v", item)
		}
	}
	if byName["system-skill"].Scope != "system" || byName["user-skill"].Scope != "user" || byName["project-a"].Scope != "project" {
		t.Fatalf("scope classification = %#v", byName)
	}
	if _, ok := byName["project-b"]; ok {
		t.Fatal("workspace B project skill leaked into workspace A inventory")
	}

	other, err := manager.SkillInventory(context.Background(), "sess_b", workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range other.Items {
		if item.Name == "project-a" {
			t.Fatal("workspace A project skill leaked into workspace B inventory")
		}
	}
}

func TestWorkspaceRegistryAndManagementInventoryStayWorkspaceScoped(t *testing.T) {
	dataRoot := t.TempDir()
	skillsDir := filepath.Join(dataRoot, "skills")
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	writeManagementSkill(t, filepath.Join(workspaceA, "skills"), "only-a")
	writeManagementSkill(t, filepath.Join(workspaceB, "skills"), "only-b")

	registry := NewWorkspaceRegistry(skillsDir)
	defer registry.Close()
	instanceA, err := registry.Get(workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	instanceB, err := registry.Get(workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := instanceA.SkillReg.Get("only-a"); !ok {
		t.Fatal("workspace registry did not load workspace A skill")
	}
	if _, ok := instanceA.SkillReg.Get("only-b"); ok {
		t.Fatal("workspace registry leaked workspace B skill")
	}
	if _, ok := instanceB.SkillReg.Get("only-b"); !ok {
		t.Fatal("workspace registry did not load workspace B skill")
	}

	manager := NewSkillPluginManager(skillsDir)
	inventoryA, err := manager.SkillInventory(context.Background(), "sess-a", workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range inventoryA.Items {
		if item.Name == "only-b" {
			t.Fatal("management inventory leaked workspace B skill")
		}
	}
}

func TestSkillPluginManagerPluginLifecycleAndActiveInventory(t *testing.T) {
	dataRoot := t.TempDir()
	skillsDir := filepath.Join(dataRoot, "skills")
	workspace := t.TempDir()
	source := writeManagementPlugin(t, "docs", "1.0.0", "Search docs", map[string]any{
		"lookup": map[string]any{"command": "docs-mcp", "env": map[string]any{"TOKEN": "super-secret-token"}},
	})
	manager := NewSkillPluginManager(skillsDir)
	ctx := context.Background()

	if _, err := manager.InstallLocalPlugin(ctx, InstallLocalPluginRequest{Source: source}); managementCode(err) != CodePluginConfirmationRequired {
		t.Fatalf("unconfirmed install error = %v", err)
	}
	managedSource := filepath.Join(skillsDir, "managed-source")
	writeManagementPluginAt(t, managedSource, "managed-source", "1.0.0", "Managed source", nil)
	if _, err := manager.InstallLocalPlugin(ctx, InstallLocalPluginRequest{Source: managedSource, Confirmed: true}); managementCode(err) != CodePluginSourceInvalid {
		t.Fatalf("managed-root source error = %v", err)
	}
	if _, err := manager.InstallLocalPlugin(ctx, InstallLocalPluginRequest{Source: "https://example.com/plugin.zip", Confirmed: true}); managementCode(err) != CodePluginSourceUnsupported {
		t.Fatalf("remote source error = %v", err)
	}
	installed, err := manager.InstallLocalPlugin(ctx, InstallLocalPluginRequest{Source: source, Confirmed: true})
	if err != nil {
		t.Fatal(err)
	}
	if installed.Action != "installed" || !installed.ReloadRequired || installed.ReloadHint == "" {
		t.Fatalf("install response = %#v", installed)
	}
	if installed.Plugin.Name != "docs" || installed.Plugin.IntegrityStatus != "verified" || len(installed.Plugin.InstalledSkills) != 1 || !installed.Plugin.InstalledSkills[0].Valid {
		t.Fatalf("plugin item = %#v", installed.Plugin)
	}
	if len(installed.Plugin.MCPServers) != 1 || installed.Plugin.MCPServers[0] != "docs__lookup" || !installed.Plugin.HasApps || !installed.Plugin.HasHooks {
		t.Fatalf("plugin capabilities = %#v", installed.Plugin)
	}

	inventory, err := manager.SkillInventory(ctx, "sess", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !inventory.ReloadRequired {
		t.Fatal("newly installed plugin should require runtime reload")
	}
	pluginSkillFound := false
	for _, item := range inventory.Items {
		if item.Name == "docs:search" {
			pluginSkillFound = true
			if item.Scope != "plugin" || item.PluginName != "docs" || !item.Enabled {
				t.Fatalf("plugin skill item = %#v", item)
			}
		}
	}
	if !pluginSkillFound {
		t.Fatal("enabled plugin did not contribute its namespaced skill")
	}

	disabled, err := manager.SetPluginEnabled(ctx, "docs", false)
	if err != nil {
		t.Fatal(err)
	}
	if !disabled.ReloadRequired || disabled.Plugin.Enabled {
		t.Fatalf("disable response = %#v", disabled)
	}
	inventory, err = manager.SkillInventory(ctx, "sess", workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range inventory.Items {
		if item.Name == "docs:search" {
			t.Fatal("disabled plugin skill appeared in active inventory")
		}
	}
	cfg, err := plugins.MCPConfigForSkillsDir(skillsDir)
	if err != nil || len(cfg.Servers) != 0 {
		t.Fatalf("disabled plugin contributed MCP: %#v, %v", cfg.Servers, err)
	}

	raw, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"super-secret-token", source, dataRoot} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("plugin mutation leaked %q: %s", forbidden, raw)
		}
	}

	registry, err := plugins.LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil || len(registry.Plugins) != 1 {
		t.Fatalf("registry = %#v, %v", registry, err)
	}
	tamperPath := filepath.Join(registry.Plugins[0].Path, "skills", "search", "SKILL.md")
	if err := os.WriteFile(tamperPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PluginInventory(ctx); managementCode(err) != CodePluginIntegrityError {
		t.Fatalf("tampered plugin inventory error = %v", err)
	}
	if _, err := manager.SkillInventory(ctx, "sess", workspace); managementCode(err) != CodePluginIntegrityError {
		t.Fatalf("tampered skill inventory error = %v", err)
	}

	removed, err := manager.UninstallPlugin(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if removed.Name != "docs" || removed.ReloadRequired {
		t.Fatalf("uninstall response = %#v", removed)
	}
	if _, err := os.Stat(registry.Plugins[0].Path); !os.IsNotExist(err) {
		t.Fatalf("plugin cache still exists after uninstall: %v", err)
	}
	registry, err = plugins.LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil || len(registry.Plugins) != 0 {
		t.Fatalf("registry after uninstall = %#v, %v", registry, err)
	}
}

func TestSkillPluginManagerReloadRequiredAndMalformedManifest(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	source := writeManagementPlugin(t, "alpha", "1.0.0", "Alpha", nil)
	if _, err := plugins.InstallLocal(skillsDir, source, plugins.InstallOptions{Confirmed: true, Source: "local_directory"}); err != nil {
		t.Fatal(err)
	}
	manager := NewSkillPluginManager(skillsDir)
	ctx := context.Background()

	disabled, err := manager.SetPluginEnabled(ctx, "alpha", false)
	if err != nil || !disabled.ReloadRequired {
		t.Fatalf("disable = %#v, %v", disabled, err)
	}
	enabled, err := manager.SetPluginEnabled(ctx, "alpha", true)
	if err != nil || enabled.ReloadRequired {
		t.Fatalf("re-enable to startup state should not require reload: %#v, %v", enabled, err)
	}

	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, ".codeagent-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, ".codeagent-plugin", "plugin.json"), []byte(`{"schema":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InstallLocalPlugin(ctx, InstallLocalPluginRequest{Source: bad, Confirmed: true}); managementCode(err) != CodePluginManifestInvalid {
		t.Fatalf("malformed manifest error = %v", err)
	}
}

func TestSkillPluginManagerManifestDigestMismatchIsIntegrityError(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	source := writeManagementPlugin(t, "manifest-tamper", "1.0.0", "Original", nil)
	entry, err := plugins.InstallLocal(skillsDir, source, plugins.InstallOptions{Confirmed: true, Source: "local_directory"})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(entry.Path, ".codeagent-plugin", "plugin.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["description"] = "Changed after install"
	changed, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewSkillPluginManager(skillsDir)
	if _, err := manager.PluginInventory(context.Background()); managementCode(err) != CodePluginIntegrityError {
		t.Fatalf("manifest digest mismatch error = %v", err)
	}
}

func writeManagementSkill(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + name + "\nversion: 1\n---\nbody"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeManagementPlugin(t *testing.T, name, version, description string, mcpServers any) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name+"-plugin")
	writeManagementPluginAt(t, root, name, version, description, mcpServers)
	return root
}

func writeManagementPluginAt(t *testing.T, root, name, version, description string, mcpServers any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".codeagent-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "skills", "search"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"schema": plugins.PluginSchemaV1, "name": name, "version": version,
		"description": description, "skills": []string{"./skills/search"},
		"apps": []string{"dashboard"}, "hooks": []string{"onload"},
	}
	if mcpServers != nil {
		manifest["mcpServers"] = mcpServers
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codeagent-plugin", "plugin.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: search\ndescription: Search docs\n---\nbody"
	if err := os.WriteFile(filepath.Join(root, "skills", "search", "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
}

func managementCode(err error) string {
	var management *ManagementError
	if errors.As(err, &management) {
		return management.Code
	}
	return ""
}
