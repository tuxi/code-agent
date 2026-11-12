package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	if byName["user-skill"].DisplayName != "user skill" || byName["user-skill"].Description != "user-skill description" ||
		byName["user-skill"].SourceGroup != "user" || byName["user-skill"].Interface == nil || byName["user-skill"].Interface.Icon != "icon-user-skill" {
		t.Fatalf("skill display metadata = %#v", byName["user-skill"])
	}
	if inventory.Warnings == nil || len(inventory.Warnings) != 0 {
		t.Fatalf("skill inventory warnings = %#v", inventory.Warnings)
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
	}, map[string]any{
		"interface": map[string]any{
			"displayName": "Docs Center", "category": "documentation",
			"capabilities": []string{"search", "summarize"},
			"composerIcon": "https://cdn.example.com/icon.svg",
			"logo":         "assets/logo.png", "brandColor": "#112233", "websiteURL": "https://example.com/docs",
			"command": "raw-command-must-not-leak", "env": map[string]any{"SECRET": "interface-secret"},
		},
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
	if installed.Plugin.DisplayName != "Docs Center" || installed.Plugin.Category != "documentation" ||
		installed.Plugin.Icon != "https://cdn.example.com/icon.svg" || installed.Plugin.Logo != "assets/logo.png" ||
		installed.Plugin.BrandColor != "#112233" || installed.Plugin.WebsiteURL != "https://example.com/docs" ||
		!reflect.DeepEqual(installed.Plugin.Capabilities, []string{"search", "summarize"}) {
		t.Fatalf("plugin interface metadata = %#v", installed.Plugin)
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
			if item.Scope != "plugin" || item.SourceGroup != "plugin" || item.PluginName != "docs" || !item.Enabled ||
				item.Description != "Search docs" || item.DisplayName != "Search Docs" ||
				item.Interface == nil || item.Interface.Icon != "search" {
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
	inventoryRaw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"super-secret-token", "docs-mcp", "raw-command-must-not-leak", "interface-secret", source, dataRoot} {
		if strings.Contains(string(raw), forbidden) || strings.Contains(string(inventoryRaw), forbidden) {
			t.Fatalf("plugin response leaked %q: mutation=%s inventory=%s", forbidden, raw, inventoryRaw)
		}
	}

	removed, err := manager.UninstallPlugin(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if removed.Name != "docs" || removed.ReloadRequired {
		t.Fatalf("uninstall response = %#v", removed)
	}
	registry, err := plugins.LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil || len(registry.Plugins) != 0 {
		t.Fatalf("registry after uninstall = %#v, %v", registry, err)
	}
}

func TestSkillPluginManagerReadOnlyInventoryToleratesTamperedPlugins(t *testing.T) {
	dataRoot := t.TempDir()
	skillsDir := filepath.Join(dataRoot, "skills")
	workspace := t.TempDir()
	healthy := writeManagementPlugin(t, "healthy", "1.0.0", "Healthy plugin", map[string]any{
		"lookup": map[string]any{"command": "healthy-mcp", "env": map[string]any{"TOKEN": "healthy-secret"}},
	})
	broken := writeManagementPlugin(t, "broken", "1.0.0", "Broken plugin", map[string]any{
		"lookup": map[string]any{"command": "broken-mcp", "env": map[string]any{"TOKEN": "broken-secret"}},
	})
	manager := NewSkillPluginManager(skillsDir)
	ctx := context.Background()
	for _, source := range []string{healthy, broken} {
		if _, err := manager.InstallLocalPlugin(ctx, InstallLocalPluginRequest{Source: source, Confirmed: true}); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := plugins.LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	var brokenEntry plugins.InstalledPluginV1
	for _, entry := range registry.Plugins {
		if entry.Name == "broken" {
			brokenEntry = entry
		}
	}
	manifestPath := filepath.Join(brokenEntry.Path, ".codeagent-plugin", "plugin.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema":"tampered"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	startupManager := NewSkillPluginManager(skillsDir)

	pluginsInventory, err := startupManager.PluginInventory(ctx)
	if err != nil {
		t.Fatalf("plugin inventory must tolerate one tampered plugin: %v", err)
	}
	if len(pluginsInventory.Items) != 2 {
		t.Fatalf("plugin inventory items = %#v", pluginsInventory.Items)
	}
	byName := make(map[string]PluginInventoryItem)
	for _, item := range pluginsInventory.Items {
		byName[item.Name] = item
	}
	if byName["broken"].IntegrityStatus != "failed" || byName["broken"].ErrorCode != CodePluginIntegrityError {
		t.Fatalf("tampered plugin item = %#v", byName["broken"])
	}
	if byName["healthy"].IntegrityStatus != "verified" || byName["healthy"].ErrorCode != "" {
		t.Fatalf("healthy plugin item = %#v", byName["healthy"])
	}

	skillsInventory, err := startupManager.SkillInventory(ctx, "sess", workspace)
	if err != nil {
		t.Fatalf("skill inventory must tolerate one tampered plugin: %v", err)
	}
	skillNames := make(map[string]bool)
	for _, item := range skillsInventory.Items {
		skillNames[item.Name] = true
	}
	if !skillNames["healthy:search"] || skillNames["broken:search"] {
		t.Fatalf("skill inventory items = %#v", skillsInventory.Items)
	}
	if len(skillsInventory.Warnings) != 1 || skillsInventory.Warnings[0].Code != CodePluginIntegrityError ||
		!strings.Contains(skillsInventory.Warnings[0].Message, "broken") {
		t.Fatalf("skill inventory warnings = %#v", skillsInventory.Warnings)
	}

	if _, err := startupManager.SetPluginEnabled(ctx, "broken", false); managementCode(err) != CodePluginIntegrityError {
		t.Fatalf("enable/disable must fail closed for tampered plugin: %v", err)
	}
	if _, err := startupManager.InstallLocalPlugin(ctx, InstallLocalPluginRequest{Source: broken, Confirmed: true}); err == nil {
		t.Fatal("install must remain fail-closed when the cached plugin was tampered")
	}

	raw, err := json.Marshal(struct {
		Plugins PluginInventory `json:"plugins"`
		Skills  SkillInventory  `json:"skills"`
	}{Plugins: pluginsInventory, Skills: skillsInventory})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{dataRoot, healthy, broken, "healthy-mcp", "broken-mcp", "healthy-secret", "broken-secret"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("inventory leaked %q: %s", forbidden, raw)
		}
	}

	removed, err := startupManager.UninstallPlugin(ctx, "broken")
	if err != nil {
		t.Fatalf("tampered plugin uninstall must succeed: %v", err)
	}
	if removed.Name != "broken" || removed.Version != "1.0.0" || !removed.ReloadRequired || removed.ReloadHint == "" {
		t.Fatalf("tampered plugin uninstall response = %#v", removed)
	}
	if _, err := os.Stat(brokenEntry.Path); !os.IsNotExist(err) {
		t.Fatalf("tampered plugin cache still exists after uninstall: %v", err)
	}
	registry, err = plugins.LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil || len(registry.Plugins) != 1 || registry.Plugins[0].Name != "healthy" {
		t.Fatalf("registry after tampered uninstall = %#v, %v", registry, err)
	}
	if _, err := startupManager.UninstallPlugin(ctx, "missing"); managementCode(err) != CodePluginNotFound {
		t.Fatalf("missing plugin uninstall error = %v", err)
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

func TestSkillPluginManagerManifestDigestMismatchMarksItemFailed(t *testing.T) {
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
	inventory, err := manager.PluginInventory(context.Background())
	if err != nil {
		t.Fatalf("manifest digest mismatch must be item-local: %v", err)
	}
	if len(inventory.Items) != 1 || inventory.Items[0].IntegrityStatus != "failed" || inventory.Items[0].ErrorCode != CodePluginIntegrityError {
		t.Fatalf("manifest digest mismatch item = %#v", inventory.Items)
	}
}

func writeManagementSkill(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	displayName := strings.ReplaceAll(name, "-", " ")
	body := "---\nname: " + name + "\ndisplay_name: " + displayName + "\ndescription: " + name + " description\nversion: 1\ninterface:\n  icon: icon-" + name + "\n---\nbody"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeManagementPlugin(t *testing.T, name, version, description string, mcpServers any, extras ...map[string]any) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name+"-plugin")
	writeManagementPluginAt(t, root, name, version, description, mcpServers, extras...)
	return root
}

func writeManagementPluginAt(t *testing.T, root, name, version, description string, mcpServers any, extras ...map[string]any) {
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
	for _, extra := range extras {
		for key, value := range extra {
			manifest[key] = value
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codeagent-plugin", "plugin.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: search\ndisplay_name: Search Docs\ndescription: Search docs\ninterface:\n  icon: search\n---\nbody"
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

func TestPluginInterfaceProjectionDropsUnsafeValues(t *testing.T) {
	item := PluginInventoryItem{}
	applyPluginInterfaceMetadata(&item, json.RawMessage(`{
		"displayName":" Safe Name ",
		"category":"docs",
		"capabilities":["search","search"," summarize "],
		"composerIcon":"lucide:search",
		"logo":"/Users/private/logo.png",
		"brandColor":"red; background:url(secret)",
		"websiteURL":"file:///Users/private/plugin.html"
	}`))
	if item.DisplayName != "Safe Name" || item.Category != "docs" || item.Icon != "lucide:search" {
		t.Fatalf("safe projection = %#v", item)
	}
	if !reflect.DeepEqual(item.Capabilities, []string{"search", "summarize"}) {
		t.Fatalf("capabilities = %#v", item.Capabilities)
	}
	if item.Logo != "" || item.BrandColor != "" || item.WebsiteURL != "" {
		t.Fatalf("unsafe interface values survived projection: %#v", item)
	}
}

func TestSkillPluginManagerUninstallRejectsUnsafeCachePath(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	source := writeManagementPlugin(t, "outside", "1.0.0", "Outside path test", nil)
	if _, err := plugins.InstallLocal(skillsDir, source, plugins.InstallOptions{Confirmed: true, Source: "local_directory"}); err != nil {
		t.Fatal(err)
	}
	manager := NewSkillPluginManager(skillsDir)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := plugins.LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	registry.Plugins[0].Path = outside
	if err := plugins.SaveInstalledRegistryForSkillsDir(skillsDir, registry); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UninstallPlugin(context.Background(), "outside"); managementCode(err) != CodePluginCachePathUnsafe {
		t.Fatalf("unsafe cache path error = %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("outside-cache content was removed: %v", err)
	}
	registry, err = plugins.LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil || len(registry.Plugins) != 1 {
		t.Fatalf("registry after refused uninstall = %#v, %v", registry, err)
	}
}
