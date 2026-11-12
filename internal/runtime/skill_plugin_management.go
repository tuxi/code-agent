package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"code-agent/internal/plugins"
	"code-agent/internal/skills"
)

const (
	SkillInventorySchemaV1  = "runtime-skill-inventory/v1"
	PluginInventorySchemaV1 = "runtime-plugin-inventory/v1"
	PluginMutationSchemaV1  = "runtime-plugin-mutation/v1"

	PluginReloadHint = "Restart the CodeAgent runtime to apply plugin changes."
)

const (
	CodePluginConfirmationRequired = "plugin_confirmation_required"
	CodePluginSourceInvalid        = "plugin_source_invalid"
	CodePluginSourceUnsupported    = "plugin_source_unsupported"
	CodePluginManifestInvalid      = "plugin_manifest_invalid"
	CodePluginIntegrityError       = "plugin_integrity_error"
	CodePluginCachePathUnsafe      = "plugin_cache_path_unsafe"
	CodePluginNotFound             = "plugin_not_found"
	CodePluginRegistryInvalid      = "plugin_registry_invalid"
	CodePluginOperationFailed      = "plugin_operation_failed"
	CodeSkillInventoryFailed       = "skill_inventory_failed"
)

// ManagementError is intentionally safe to map onto the wire: Error() never
// includes the wrapped filesystem error, which may contain an absolute path.
type ManagementError struct {
	Code    string
	Message string
	Cause   error
}

func (e *ManagementError) Error() string { return e.Message }
func (e *ManagementError) Unwrap() error { return e.Cause }

func managementError(code, message string, cause error) error {
	return &ManagementError{Code: code, Message: message, Cause: cause}
}

type SkillInventoryScope struct {
	SessionID   string `json:"session_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Resolved    bool   `json:"resolved"`
}

type SkillResourceInventory struct {
	Count int      `json:"count"`
	Names []string `json:"names"`
}

type SkillInventoryItem struct {
	Name        string                   `json:"name"`
	DisplayName string                   `json:"display_name"`
	Description string                   `json:"description"`
	Scope       string                   `json:"scope"`
	Source      string                   `json:"source"`
	SourceGroup string                   `json:"source_group"`
	Path        string                   `json:"path"`
	Version     string                   `json:"version"`
	SHA256      string                   `json:"sha256"`
	Enabled     bool                     `json:"enabled"`
	Status      string                   `json:"status"`
	PluginName  string                   `json:"plugin_name,omitempty"`
	Interface   *SkillInterfaceInventory `json:"interface,omitempty"`
	Resources   SkillResourceInventory   `json:"resources"`
	Warnings    []ManagementWarning      `json:"warnings"`
	ParseErrors []string                 `json:"parse_errors"`
	ShadowedBy  *SkillOriginInventory    `json:"shadowed_by,omitempty"`
	Shadows     []SkillOriginInventory   `json:"shadows,omitempty"`
}

type SkillInterfaceInventory struct {
	Icon string `json:"icon,omitempty"`
}

type SkillOriginInventory struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Source string `json:"source"`
	Path   string `json:"path"`
}

type ManagementWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type SkillInventory struct {
	Schema         string               `json:"schema"`
	Scope          SkillInventoryScope  `json:"scope"`
	Items          []SkillInventoryItem `json:"items"`
	Warnings       []ManagementWarning  `json:"warnings"`
	ReloadRequired bool                 `json:"reload_required"`
	ReloadHint     string               `json:"reload_hint,omitempty"`
}

type PluginInstalledSkill struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Valid     bool   `json:"valid"`
	ErrorCode string `json:"error_code,omitempty"`
}

type PluginInventoryItem struct {
	Name            string                 `json:"name"`
	Version         string                 `json:"version"`
	Description     string                 `json:"description"`
	DisplayName     string                 `json:"display_name,omitempty"`
	Category        string                 `json:"category,omitempty"`
	Capabilities    []string               `json:"capabilities,omitempty"`
	Icon            string                 `json:"icon,omitempty"`
	Logo            string                 `json:"logo,omitempty"`
	BrandColor      string                 `json:"brand_color,omitempty"`
	WebsiteURL      string                 `json:"website_url,omitempty"`
	Enabled         bool                   `json:"enabled"`
	Source          string                 `json:"source"`
	SourceKind      string                 `json:"source_kind"`
	CacheID         string                 `json:"cache_id"`
	CachePath       string                 `json:"cache_path"`
	ManifestSHA256  string                 `json:"manifest_sha256"`
	BundleSHA256    string                 `json:"bundle_sha256"`
	InstalledSkills []PluginInstalledSkill `json:"installed_skills"`
	MCPServers      []string               `json:"mcp_servers"`
	HasApps         bool                   `json:"has_apps"`
	HasHooks        bool                   `json:"has_hooks"`
	IntegrityStatus string                 `json:"integrity_status"`
	ErrorCode       string                 `json:"error_code,omitempty"`
	ReloadRequired  bool                   `json:"reload_required"`
}

type PluginInventory struct {
	Schema         string                `json:"schema"`
	Items          []PluginInventoryItem `json:"items"`
	ReloadRequired bool                  `json:"reload_required"`
	ReloadHint     string                `json:"reload_hint,omitempty"`
}

type InstallLocalPluginRequest struct {
	Source    string `json:"source"`
	Confirmed bool   `json:"confirmed"`
}

type PluginMutation struct {
	Schema         string              `json:"schema"`
	Action         string              `json:"action"`
	Plugin         PluginInventoryItem `json:"plugin"`
	ReloadRequired bool                `json:"reload_required"`
	ReloadHint     string              `json:"reload_hint,omitempty"`
}

type UninstallPluginMutation struct {
	Schema         string `json:"schema"`
	Action         string `json:"action"`
	Name           string `json:"name"`
	Version        string `json:"version,omitempty"`
	ReloadRequired bool   `json:"reload_required"`
	ReloadHint     string `json:"reload_hint,omitempty"`
}

// SkillPluginManager is the runtime-facing implementation of the management
// service. It reads the same skill/plugin paths as WorkspaceRegistry and only
// mutates the versioned plugin registry/cache.
type SkillPluginManager struct {
	skillsDir string
	mu        sync.Mutex
	loaded    map[string]string
}

func NewSkillPluginManager(skillsDir string) *SkillPluginManager {
	loaded, _ := pluginStateSnapshot(skillsDir)
	return &SkillPluginManager{skillsDir: skillsDir, loaded: loaded}
}

func (m *SkillPluginManager) SkillInventory(ctx context.Context, sessionID, workspacePath string) (SkillInventory, error) {
	if err := ctx.Err(); err != nil {
		return SkillInventory{}, err
	}
	opts := skills.WorkspaceLoadOptions(m.skillsDir, workspacePath)
	inventoryWarnings := []ManagementWarning{}
	if m.skillsDir != "" {
		roots, warnings, err := m.activePluginSkillRoots()
		if err != nil {
			return SkillInventory{}, err
		}
		opts.Plugins = roots
		inventoryWarnings = append(inventoryWarnings, warnings...)
	}
	entries, err := skills.InspectScoped(opts)
	if err != nil {
		return SkillInventory{}, managementError(CodeSkillInventoryFailed, "skill inventory could not be read", err)
	}

	items := make([]SkillInventoryItem, 0, len(entries))
	for _, entry := range entries {
		displayName := strings.TrimSpace(entry.DisplayName)
		if displayName == "" {
			displayName = entry.Name
		}
		item := SkillInventoryItem{
			Name:        entry.Name,
			DisplayName: displayName,
			Description: entry.Description,
			Scope:       string(entry.Scope),
			Source:      entry.Source,
			SourceGroup: skillSourceGroup(entry.Scope, entry.Source),
			Path:        m.displayPath(workspacePath, entry.Path, entry.Scope),
			Version:     entry.Version,
			SHA256:      entry.ContentHash,
			Enabled:     entry.Enabled,
			Status:      entry.Status,
			PluginName:  entry.PluginName,
			Interface:   skillInterfaceInventory(entry.Interface),
			Resources:   m.resourceInventory(entry),
			Warnings:    inventoryWarningDTOs(entry),
			ParseErrors: append([]string{}, entry.ParseErrors...),
		}
		if entry.ShadowedBy != nil {
			item.ShadowedBy = m.originInventory(workspacePath, *entry.ShadowedBy)
		}
		for _, shadow := range entry.Shadows {
			item.Shadows = append(item.Shadows, *m.originInventory(workspacePath, shadow))
		}
		items = append(items, item)
	}
	reloadRequired, err := m.reloadRequired()
	if err != nil {
		return SkillInventory{}, managementError(CodePluginRegistryInvalid, "plugin registry could not be read", err)
	}
	result := SkillInventory{
		Schema: SkillInventorySchemaV1,
		Scope: SkillInventoryScope{
			SessionID: sessionID, WorkspaceID: opaqueID("ws", workspacePath),
			DisplayName: workspaceDisplayName(workspacePath), Resolved: workspacePath != "",
		},
		Items: items, Warnings: inventoryWarnings, ReloadRequired: reloadRequired,
	}
	if reloadRequired {
		result.ReloadHint = PluginReloadHint
	}
	return result, nil
}

func (m *SkillPluginManager) PluginInventory(ctx context.Context) (PluginInventory, error) {
	if err := ctx.Err(); err != nil {
		return PluginInventory{}, err
	}
	if strings.TrimSpace(m.skillsDir) == "" {
		return PluginInventory{}, managementError(CodePluginRegistryInvalid, "plugin management is not configured", nil)
	}
	registry, err := plugins.LoadInstalledRegistryForSkillsDir(m.skillsDir)
	if err != nil {
		return PluginInventory{}, managementError(CodePluginRegistryInvalid, "plugin registry could not be read", err)
	}
	items := make([]PluginInventoryItem, 0, len(registry.Plugins))
	for _, entry := range registry.Plugins {
		item, err := m.pluginInventoryItem(entry)
		if err != nil {
			return PluginInventory{}, err
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	reloadRequired, err := m.reloadRequired()
	if err != nil {
		return PluginInventory{}, managementError(CodePluginRegistryInvalid, "plugin registry could not be read", err)
	}
	result := PluginInventory{Schema: PluginInventorySchemaV1, Items: items, ReloadRequired: reloadRequired}
	if reloadRequired {
		result.ReloadHint = PluginReloadHint
	}
	return result, nil
}

func (m *SkillPluginManager) SetPluginEnabled(ctx context.Context, name string, enabled bool) (PluginMutation, error) {
	if err := ctx.Err(); err != nil {
		return PluginMutation{}, err
	}
	if strings.TrimSpace(m.skillsDir) == "" {
		return PluginMutation{}, managementError(CodePluginRegistryInvalid, "plugin management is not configured", nil)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return PluginMutation{}, managementError(CodePluginNotFound, "plugin name is required", nil)
	}
	registry, err := plugins.LoadInstalledRegistryForSkillsDir(m.skillsDir)
	if err != nil {
		return PluginMutation{}, managementError(CodePluginRegistryInvalid, "plugin registry could not be read", err)
	}
	entry, ok := installedPluginByName(registry, name)
	if !ok {
		return PluginMutation{}, managementError(CodePluginNotFound, "plugin is not installed", plugins.ErrPluginNotFound)
	}
	if _, err := plugins.VerifyInstalled(entry); err != nil {
		return PluginMutation{}, managementError(CodePluginIntegrityError, "installed plugin failed integrity verification", err)
	}
	if err := plugins.SetPluginEnabled(m.skillsDir, name, enabled); err != nil {
		if errorsIsPluginNotFound(err) {
			return PluginMutation{}, managementError(CodePluginNotFound, "plugin is not installed", err)
		}
		return PluginMutation{}, managementError(CodePluginOperationFailed, "plugin state could not be updated", err)
	}
	registry, err = plugins.LoadInstalledRegistryForSkillsDir(m.skillsDir)
	if err != nil {
		return PluginMutation{}, managementError(CodePluginRegistryInvalid, "plugin registry could not be read", err)
	}
	updated, ok := installedPluginByName(registry, name)
	if !ok {
		return PluginMutation{}, managementError(CodePluginNotFound, "plugin is not installed", plugins.ErrPluginNotFound)
	}
	item, err := m.pluginInventoryItem(updated)
	if err != nil {
		return PluginMutation{}, err
	}
	reloadRequired, err := m.pluginReloadRequired(name)
	if err != nil {
		return PluginMutation{}, err
	}
	action := "disabled"
	if enabled {
		action = "enabled"
	}
	result := PluginMutation{Schema: PluginMutationSchemaV1, Action: action, Plugin: item, ReloadRequired: reloadRequired}
	if reloadRequired {
		result.ReloadHint = PluginReloadHint
	}
	return result, nil
}

func (m *SkillPluginManager) InstallLocalPlugin(ctx context.Context, req InstallLocalPluginRequest) (PluginMutation, error) {
	if err := ctx.Err(); err != nil {
		return PluginMutation{}, err
	}
	if strings.TrimSpace(m.skillsDir) == "" {
		return PluginMutation{}, managementError(CodePluginRegistryInvalid, "plugin management is not configured", nil)
	}
	if !req.Confirmed {
		return PluginMutation{}, managementError(CodePluginConfirmationRequired, "explicit confirmation is required to install a local plugin", plugins.ErrConfirmationRequired)
	}
	source := strings.TrimSpace(req.Source)
	if source == "" || strings.HasPrefix(source, "//") {
		return PluginMutation{}, managementError(CodePluginSourceInvalid, "plugin source must be a local directory or .zip path", nil)
	}
	if parsed, err := url.Parse(source); err == nil && parsed.Scheme != "" {
		return PluginMutation{}, managementError(CodePluginSourceUnsupported, "remote plugin sources are not supported", nil)
	}
	if !filepath.IsAbs(source) {
		return PluginMutation{}, managementError(CodePluginSourceInvalid, "plugin source must be an absolute local path", nil)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return PluginMutation{}, managementError(CodePluginSourceInvalid, "plugin source does not exist or is not accessible", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return PluginMutation{}, managementError(CodePluginSourceInvalid, "plugin source cannot be a symlink", nil)
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return PluginMutation{}, managementError(CodePluginSourceInvalid, "plugin source could not be resolved safely", err)
	}
	info, err = os.Stat(source)
	if err != nil {
		return PluginMutation{}, managementError(CodePluginSourceInvalid, "plugin source does not exist or is not accessible", err)
	}
	if !info.IsDir() && !(info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(source), ".zip")) {
		return PluginMutation{}, managementError(CodePluginSourceUnsupported, "plugin source must be a local directory or .zip archive", nil)
	}
	if m.sourceInsideManagedRoot(source) {
		return PluginMutation{}, managementError(CodePluginSourceInvalid, "plugin source cannot be inside the managed skills or plugin cache roots", nil)
	}
	sourceKind := "local_directory"
	if !info.IsDir() {
		sourceKind = "local_zip"
	}
	entry, err := plugins.InstallLocal(m.skillsDir, source, plugins.InstallOptions{Confirmed: true, Source: sourceKind})
	if err != nil {
		switch {
		case errorsIsConfirmation(err):
			return PluginMutation{}, managementError(CodePluginConfirmationRequired, "explicit confirmation is required to install a local plugin", err)
		case errorsIsManifestInvalid(err):
			return PluginMutation{}, managementError(CodePluginManifestInvalid, "plugin manifest is malformed or invalid", err)
		default:
			return PluginMutation{}, managementError(CodePluginOperationFailed, "local plugin could not be installed", err)
		}
	}
	item, err := m.pluginInventoryItem(*entry)
	if err != nil {
		return PluginMutation{}, err
	}
	reloadRequired, err := m.pluginReloadRequired(entry.Name)
	if err != nil {
		return PluginMutation{}, err
	}
	result := PluginMutation{Schema: PluginMutationSchemaV1, Action: "installed", Plugin: item, ReloadRequired: reloadRequired}
	if reloadRequired {
		result.ReloadHint = PluginReloadHint
	}
	return result, nil
}

func (m *SkillPluginManager) UninstallPlugin(ctx context.Context, name string) (UninstallPluginMutation, error) {
	if err := ctx.Err(); err != nil {
		return UninstallPluginMutation{}, err
	}
	if strings.TrimSpace(m.skillsDir) == "" {
		return UninstallPluginMutation{}, managementError(CodePluginRegistryInvalid, "plugin management is not configured", nil)
	}
	name = strings.TrimSpace(name)
	registry, err := plugins.LoadInstalledRegistryForSkillsDir(m.skillsDir)
	if err != nil {
		return UninstallPluginMutation{}, managementError(CodePluginRegistryInvalid, "plugin registry could not be read", err)
	}
	entry, ok := installedPluginByName(registry, name)
	if !ok {
		return UninstallPluginMutation{}, managementError(CodePluginNotFound, "plugin is not installed", plugins.ErrPluginNotFound)
	}
	if err := plugins.UninstallLocal(m.skillsDir, name); err != nil {
		if errorsIsPluginNotFound(err) {
			return UninstallPluginMutation{}, managementError(CodePluginNotFound, "plugin is not installed", err)
		}
		if errorsIsUnsafePluginCachePath(err) {
			return UninstallPluginMutation{}, managementError(CodePluginCachePathUnsafe, "plugin cache path failed safety checks", err)
		}
		return UninstallPluginMutation{}, managementError(CodePluginOperationFailed, "plugin could not be uninstalled", err)
	}
	reloadRequired, err := m.pluginReloadRequired(name)
	if err != nil {
		return UninstallPluginMutation{}, err
	}
	result := UninstallPluginMutation{
		Schema: PluginMutationSchemaV1, Action: "uninstalled", Name: name,
		Version: entry.Version, ReloadRequired: reloadRequired,
	}
	if reloadRequired {
		result.ReloadHint = PluginReloadHint
	}
	return result, nil
}

func (m *SkillPluginManager) pluginInventoryItem(entry plugins.InstalledPluginV1) (PluginInventoryItem, error) {
	item, err := m.basePluginInventoryItem(entry)
	if err != nil {
		return PluginInventoryItem{}, err
	}
	manifest, err := plugins.VerifyInstalled(entry)
	if err != nil {
		item.IntegrityStatus = "failed"
		item.ErrorCode = CodePluginIntegrityError
		return item, nil
	}
	item.IntegrityStatus = "verified"
	item.InstalledSkills = m.pluginInstalledSkills(entry, manifest)
	item.HasApps = jsonFieldPresent(manifest.Apps)
	item.HasHooks = jsonFieldPresent(manifest.Hooks)
	applyPluginInterfaceMetadata(&item, manifest.Interface)
	mcpNames, err := plugins.MCPConfigForManifest(entry.Path, entry.Name, manifest)
	if err != nil {
		item.IntegrityStatus = "failed"
		item.ErrorCode = "plugin_mcp_config_invalid"
		return item, nil
	}
	servers := make([]string, 0, len(mcpNames.Servers))
	for _, server := range mcpNames.Servers {
		servers = append(servers, plugins.NamespacedMCPServerName(entry.Name, server.Name))
	}
	sort.Strings(servers)
	item.MCPServers = servers
	return item, nil
}

func (m *SkillPluginManager) basePluginInventoryItem(entry plugins.InstalledPluginV1) (PluginInventoryItem, error) {
	reloadRequired, err := m.pluginReloadRequired(entry.Name)
	if err != nil {
		return PluginInventoryItem{}, err
	}
	cachePath := m.displayPath("", entry.Path, "")
	if !strings.HasPrefix(cachePath, "plugins/cache/") {
		cachePath = filepath.ToSlash(filepath.Join("plugins", "cache", entry.Name, entry.Version))
	}
	return PluginInventoryItem{
		Name: entry.Name, Version: entry.Version, Description: entry.Description,
		Enabled: entry.Enabled, Source: "local", SourceKind: pluginSourceKind(entry.Source),
		CacheID:   opaqueID("plg", entry.Name+"@"+entry.Version+":"+entry.ManifestSHA256+":"+entry.BundleSHA256),
		CachePath: cachePath, ManifestSHA256: entry.ManifestSHA256, BundleSHA256: entry.BundleSHA256,
		InstalledSkills: []PluginInstalledSkill{}, MCPServers: []string{},
		IntegrityStatus: "verified", ReloadRequired: reloadRequired,
	}, nil
}

func (m *SkillPluginManager) pluginInstalledSkills(entry plugins.InstalledPluginV1, manifest *plugins.Manifest) []PluginInstalledSkill {
	skillsOut := make([]PluginInstalledSkill, 0, len(manifest.Skills))
	for _, declared := range manifest.Skills {
		root, err := plugins.ResolvePluginPath(entry.Path, declared)
		if err != nil {
			skillsOut = append(skillsOut, PluginInstalledSkill{Name: entry.Name + ":" + filepath.Base(declared), Path: filepath.ToSlash(declared), ErrorCode: "plugin_skill_path_invalid"})
			continue
		}
		skillFile := filepath.Join(root, "SKILL.md")
		data, readErr := os.ReadFile(skillFile)
		meta, parseErr := skills.ParseMeta(data)
		item := PluginInstalledSkill{
			Name: entry.Name + ":" + filepath.Base(root),
			Path: m.displayPath("", skillFile, skills.ScopePlugin),
		}
		switch {
		case readErr != nil:
			item.ErrorCode = "plugin_skill_unreadable"
		case parseErr != nil:
			item.ErrorCode = "plugin_skill_invalid"
		default:
			item.Name = entry.Name + ":" + meta.Name
			item.Valid = true
		}
		skillsOut = append(skillsOut, item)
	}
	sort.Slice(skillsOut, func(i, j int) bool { return skillsOut[i].Name < skillsOut[j].Name })
	return skillsOut
}

// activePluginSkillRoots verifies plugins independently so one corrupt cache
// cannot hide skills from healthy plugins. Disabled plugins are not roots.
func (m *SkillPluginManager) activePluginSkillRoots() ([]skills.Root, []ManagementWarning, error) {
	registry, err := plugins.LoadInstalledRegistryForSkillsDir(m.skillsDir)
	if err != nil {
		return nil, nil, managementError(CodePluginRegistryInvalid, "plugin registry could not be read", err)
	}
	roots := make([]skills.Root, 0)
	warnings := make([]ManagementWarning, 0)
	for _, entry := range registry.Plugins {
		if !entry.Enabled {
			continue
		}
		manifest, err := plugins.VerifyInstalled(entry)
		if err != nil {
			warnings = append(warnings, ManagementWarning{
				Code:    CodePluginIntegrityError,
				Message: fmt.Sprintf("plugin %q was skipped because its integrity check failed", entry.Name),
			})
			continue
		}
		for _, declared := range manifest.Skills {
			root, err := plugins.ResolvePluginPath(entry.Path, declared)
			if err != nil {
				warnings = append(warnings, ManagementWarning{
					Code:    "plugin_skill_invalid",
					Message: fmt.Sprintf("plugin %q declared an invalid skill root and it was skipped", entry.Name),
				})
				continue
			}
			roots = append(roots, skills.Root{Dir: root, Scope: skills.ScopePlugin, PluginName: entry.Name})
		}
	}
	return roots, warnings, nil
}

func skillSourceGroup(scope skills.Scope, source string) string {
	if scope == skills.ScopePlugin || strings.HasPrefix(source, "plugin:") {
		return "plugin"
	}
	return string(scope)
}

func skillInterfaceInventory(metadata skills.Interface) *SkillInterfaceInventory {
	icon := safeInterfaceAsset(metadata.Icon)
	if icon == "" {
		return nil
	}
	return &SkillInterfaceInventory{Icon: icon}
}

func applyPluginInterfaceMetadata(item *PluginInventoryItem, raw json.RawMessage) {
	metadata, ok := plugins.ParseInterfaceMetadata(raw)
	if !ok {
		return
	}
	item.DisplayName = safeDisplayText(metadata.DisplayName, 80)
	item.Category = safeDisplayText(metadata.Category, 80)
	item.Capabilities = safeCapabilities(metadata.Capabilities)
	item.Icon = safeInterfaceAsset(metadata.Icon)
	item.Logo = safeInterfaceAsset(metadata.Logo)
	item.BrandColor = safeBrandColor(metadata.BrandColor)
	item.WebsiteURL = safeHTTPURL(metadata.WebsiteURL)
}

func safeDisplayText(value string, max int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max])
	}
	return value
}

func safeCapabilities(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = safeDisplayText(value, 64)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if len(out) == 32 {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func safeInterfaceAsset(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 || strings.Contains(value, "\\") || strings.ContainsAny(value, " \t\n\r") {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		if (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil {
			return value
		}
		if !strings.ContainsAny(value, `/\\`) && safeInterfaceToken(value) {
			return value
		}
		return ""
	}
	if filepath.IsAbs(value) || strings.Contains(value, "..") {
		return ""
	}
	return value
}

func safeInterfaceToken(value string) bool {
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func safeHTTPURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	return value
}

func safeBrandColor(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 4 && len(value) != 5 && len(value) != 7 && len(value) != 9 {
		return ""
	}
	if value[0] != '#' {
		return ""
	}
	for _, r := range value[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ""
		}
	}
	return value
}

func (m *SkillPluginManager) resourceInventory(entry skills.InventoryEntry) SkillResourceInventory {
	names := make([]string, 0, len(entry.Resources))
	for _, resource := range entry.Resources {
		name := filepath.Base(resource.Path)
		if entry.Dir != "" {
			if rel, err := filepath.Rel(entry.Dir, resource.Path); err == nil && safeRelative(rel) {
				name = filepath.ToSlash(rel)
			}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return SkillResourceInventory{Count: len(names), Names: names}
}

func inventoryWarningDTOs(entry skills.InventoryEntry) []ManagementWarning {
	warnings := make([]ManagementWarning, 0, len(entry.Warnings)+1)
	if len(entry.Shadows) > 0 && len(entry.Warnings) > 0 {
		warnings = append(warnings, ManagementWarning{Code: "skill_shadows", Message: entry.Warnings[len(entry.Warnings)-1]})
	}
	if entry.Status == skills.InventoryStatusShadowed && len(entry.Warnings) > 0 {
		warnings = append(warnings, ManagementWarning{Code: "skill_shadowed", Message: entry.Warnings[0]})
	}
	if entry.Status == skills.InventoryStatusInvalid && len(entry.Warnings) > 0 {
		warnings = append(warnings, ManagementWarning{Code: "skill_parse_error", Message: entry.Warnings[0]})
	}
	if len(warnings) == 0 {
		return []ManagementWarning{}
	}
	return warnings
}

func (m *SkillPluginManager) originInventory(workspacePath string, origin skills.SkillOrigin) *SkillOriginInventory {
	return &SkillOriginInventory{
		Name: origin.Name, Scope: string(origin.Scope), Source: origin.Source,
		Path: m.displayPath(workspacePath, origin.Path, origin.Scope),
	}
}

func (m *SkillPluginManager) displayPath(workspacePath, absolute string, scope skills.Scope) string {
	if absolute == "" {
		return ""
	}
	base := filepath.Dir(m.skillsDir)
	if scope == skills.ScopeProject && workspacePath != "" {
		base = workspacePath
	}
	if base != "" {
		if rel, err := filepath.Rel(base, absolute); err == nil && safeRelative(rel) {
			return filepath.ToSlash(rel)
		}
	}
	return "opaque:" + opaqueID("path", absolute)
}

func (m *SkillPluginManager) reloadRequired() (bool, error) {
	if strings.TrimSpace(m.skillsDir) == "" {
		return false, nil
	}
	current, err := pluginStateSnapshot(m.skillsDir)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	loaded := cloneStringMap(m.loaded)
	m.mu.Unlock()
	if len(current) != len(loaded) {
		return true, nil
	}
	for name, state := range current {
		if loaded[name] != state {
			return true, nil
		}
	}
	return false, nil
}

func (m *SkillPluginManager) pluginReloadRequired(name string) (bool, error) {
	if strings.TrimSpace(m.skillsDir) == "" {
		return false, nil
	}
	current, err := pluginStateSnapshot(m.skillsDir)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	loadedState, loaded := m.loaded[name]
	m.mu.Unlock()
	currentState, exists := current[name]
	if !exists {
		return loaded, nil
	}
	return !loaded || currentState != loadedState, nil
}

func pluginStateSnapshot(skillsDir string) (map[string]string, error) {
	registry, err := plugins.LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil {
		return nil, err
	}
	states := make(map[string]string, len(registry.Plugins))
	for _, entry := range registry.Plugins {
		enabled := "0"
		if entry.Enabled {
			enabled = "1"
		}
		states[entry.Name] = enabled + ":" + entry.Version + ":" + entry.ManifestSHA256 + ":" + entry.BundleSHA256
	}
	return states, nil
}

func installedPluginByName(registry *plugins.InstalledRegistryV1, name string) (plugins.InstalledPluginV1, bool) {
	for _, entry := range registry.Plugins {
		if entry.Name == name {
			return entry, true
		}
	}
	return plugins.InstalledPluginV1{}, false
}

func (m *SkillPluginManager) sourceInsideManagedRoot(source string) bool {
	if m.skillsDir == "" {
		return false
	}
	skillsRoot, err := filepath.Abs(m.skillsDir)
	if err != nil {
		return true
	}
	if resolved, resolveErr := filepath.EvalSymlinks(skillsRoot); resolveErr == nil {
		skillsRoot = resolved
	}
	pluginsRoot, err := filepath.Abs(filepath.Join(plugins.PluginsRootForSkillsDir(m.skillsDir), "cache"))
	if err != nil {
		return true
	}
	if resolved, resolveErr := filepath.EvalSymlinks(pluginsRoot); resolveErr == nil {
		pluginsRoot = resolved
	}
	return pathInside(skillsRoot, source) || pathInside(pluginsRoot, source) || source == skillsRoot || source == pluginsRoot
}

func pathInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != "." && safeRelative(rel)
}

func safeRelative(path string) bool {
	return path != "" && path != "." && path != ".." && !strings.Contains(path, "\\") &&
		!strings.HasPrefix(path, ".."+string(filepath.Separator)) && !filepath.IsAbs(path)
}

func workspaceDisplayName(workspacePath string) string {
	if workspacePath == "" {
		return ""
	}
	clean := filepath.Clean(workspacePath)
	name := filepath.Base(clean)
	if name == "." || name == string(filepath.Separator) {
		return clean
	}
	return name
}

func opaqueID(prefix, value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

func pluginSourceKind(source string) string {
	switch source {
	case "local_directory", "local_zip":
		return source
	}
	if strings.EqualFold(filepath.Ext(source), ".zip") {
		return "local_zip"
	}
	return "local_directory"
}

func jsonFieldPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func errorsIsConfirmation(err error) bool {
	return managementErrorsIs(err, plugins.ErrConfirmationRequired)
}

func errorsIsManifestInvalid(err error) bool {
	return managementErrorsIs(err, plugins.ErrManifestInvalid)
}

func errorsIsPluginNotFound(err error) bool {
	return managementErrorsIs(err, plugins.ErrPluginNotFound)
}

func errorsIsUnsafePluginCachePath(err error) bool {
	return managementErrorsIs(err, plugins.ErrUnsafePluginCachePath)
}

func managementErrorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
