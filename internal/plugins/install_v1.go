package plugins

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrConfirmationRequired = errors.New("plugin installation requires explicit user confirmation")
	ErrManifestInvalid      = errors.New("plugin manifest is invalid")
	ErrPluginNotFound       = errors.New("plugin is not installed")
)

type InstallOptions struct {
	Confirmed bool
	Source    string
}

type InstalledPluginV1 struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Description    string `json:"description"`
	Path           string `json:"path"`
	Source         string `json:"source"`
	ManifestSHA256 string `json:"manifest_sha256"`
	BundleSHA256   string `json:"bundle_sha256"`
	Enabled        bool   `json:"enabled"`
	InstalledAt    string `json:"installed_at"`
}

type InstalledRegistryV1 struct {
	Schema  string              `json:"schema"`
	Plugins []InstalledPluginV1 `json:"plugins"`
}

const installedRegistrySchema = "codeagent.installed-plugins.v1"

func PluginsRootForSkillsDir(skillsDir string) string {
	return filepath.Join(filepath.Dir(skillsDir), "plugins")
}

func InstalledRegistryPathForSkillsDir(skillsDir string) string {
	return filepath.Join(PluginsRootForSkillsDir(skillsDir), "installed-plugins.json")
}

func LoadInstalledRegistryForSkillsDir(skillsDir string) (*InstalledRegistryV1, error) {
	path := InstalledRegistryPathForSkillsDir(skillsDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &InstalledRegistryV1{Schema: installedRegistrySchema}, nil
		}
		return nil, fmt.Errorf("read installed plugin registry: %w", err)
	}
	var registry InstalledRegistryV1
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("parse installed plugin registry: %w", err)
	}
	if registry.Schema != installedRegistrySchema {
		return nil, fmt.Errorf("unsupported installed plugin registry schema %q", registry.Schema)
	}
	return &registry, nil
}

func SaveInstalledRegistryForSkillsDir(skillsDir string, registry *InstalledRegistryV1) error {
	if registry.Schema == "" {
		registry.Schema = installedRegistrySchema
	}
	sort.Slice(registry.Plugins, func(i, j int) bool { return registry.Plugins[i].Name < registry.Plugins[j].Name })
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := InstalledRegistryPathForSkillsDir(skillsDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	stage, err := os.CreateTemp(filepath.Dir(path), ".installed-plugins.tmp-*")
	if err != nil {
		return err
	}
	name := stage.Name()
	defer os.Remove(name)
	if err := stage.Chmod(0o644); err != nil {
		stage.Close()
		return err
	}
	if _, err := stage.Write(data); err != nil {
		stage.Close()
		return err
	}
	if err := stage.Sync(); err != nil {
		stage.Close()
		return err
	}
	if err := stage.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// InstallLocal installs a local directory or .zip only. Remote marketplace and
// automatic download are deliberately not part of this contract.
func InstallLocal(skillsDir, source string, opts InstallOptions) (*InstalledPluginV1, error) {
	if !opts.Confirmed {
		return nil, ErrConfirmationRequired
	}
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(sourceAbs)
	if err != nil {
		return nil, fmt.Errorf("plugin source: %w", err)
	}
	cacheRoot := filepath.Join(PluginsRootForSkillsDir(skillsDir), "cache")
	if info, statErr := os.Lstat(cacheRoot); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("plugin cache root must be a real directory")
		}
	}
	stageRoot, err := os.MkdirTemp(cacheRoot, ".staging-")
	if err != nil {
		if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
			return nil, err
		}
		info, statErr := os.Lstat(cacheRoot)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("plugin cache root must be a real directory")
		}
		stageRoot, err = os.MkdirTemp(cacheRoot, ".staging-")
		if err != nil {
			return nil, err
		}
	}
	defer os.RemoveAll(stageRoot)
	stage := filepath.Join(stageRoot, "plugin")
	switch {
	case info.IsDir():
		if err := copyPluginTree(sourceAbs, stage); err != nil {
			return nil, err
		}
	case info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(sourceAbs), ".zip"):
		if err := extractPluginZip(sourceAbs, stage); err != nil {
			return nil, err
		}
		if err := normalizeSingleRoot(stage); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("plugin source must be a local directory or .zip archive")
	}

	manifest, manifestPath, err := LoadManifest(stage)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	manifestDigest := hashBytes(manifestData)
	bundleDigest, err := digestTree(stage)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(cacheRoot, manifest.Name, manifest.Version)
	if existingInfo, err := os.Stat(final); err == nil && existingInfo.IsDir() {
		existingDigest, digestErr := digestTree(final)
		if digestErr != nil {
			return nil, digestErr
		}
		if existingDigest != bundleDigest {
			return nil, fmt.Errorf("plugin %s@%s is already cached with different content", manifest.Name, manifest.Version)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(stage, final); err != nil {
			return nil, fmt.Errorf("activate plugin cache: %w", err)
		}
	}

	registry, err := LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil {
		return nil, err
	}
	entry := InstalledPluginV1{
		Name:           manifest.Name,
		Version:        manifest.Version,
		Description:    manifest.Description,
		Path:           final,
		Source:         opts.Source,
		ManifestSHA256: manifestDigest,
		BundleSHA256:   bundleDigest,
		Enabled:        true,
		InstalledAt:    time.Now().UTC().Format(time.RFC3339),
	}
	replaced := false
	for i := range registry.Plugins {
		if registry.Plugins[i].Name == entry.Name {
			registry.Plugins[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		registry.Plugins = append(registry.Plugins, entry)
	}
	if err := SaveInstalledRegistryForSkillsDir(skillsDir, registry); err != nil {
		return nil, err
	}
	return &entry, nil
}

func normalizeSingleRoot(stage string) error {
	if _, err := FindManifest(stage); err == nil {
		return nil
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	nested := filepath.Join(stage, entries[0].Name())
	if _, err := FindManifest(nested); err != nil {
		return nil
	}
	temporary := stage + ".root"
	if err := os.Rename(nested, temporary); err != nil {
		return err
	}
	if err := os.Remove(stage); err != nil {
		return err
	}
	return os.Rename(temporary, stage)
}

func SetPluginEnabled(skillsDir, name string, enabled bool) error {
	registry, err := LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil {
		return err
	}
	for i := range registry.Plugins {
		if registry.Plugins[i].Name == name {
			registry.Plugins[i].Enabled = enabled
			return SaveInstalledRegistryForSkillsDir(skillsDir, registry)
		}
	}
	return fmt.Errorf("%w: %q", ErrPluginNotFound, name)
}

func UninstallLocal(skillsDir, name string) error {
	registry, err := LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil {
		return err
	}
	var remaining []InstalledPluginV1
	var removed *InstalledPluginV1
	for i := range registry.Plugins {
		if registry.Plugins[i].Name == name {
			removed = &registry.Plugins[i]
			continue
		}
		remaining = append(remaining, registry.Plugins[i])
	}
	if removed == nil {
		return fmt.Errorf("%w: %q", ErrPluginNotFound, name)
	}
	cacheRoot := filepath.Join(PluginsRootForSkillsDir(skillsDir), "cache")
	cacheInfo, err := os.Lstat(cacheRoot)
	if err != nil {
		return fmt.Errorf("inspect plugin cache root: %w", err)
	}
	if !cacheInfo.IsDir() || cacheInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to use a non-directory or symlinked plugin cache root")
	}
	cacheRoot, err = filepath.EvalSymlinks(cacheRoot)
	if err != nil {
		return fmt.Errorf("resolve plugin cache root: %w", err)
	}
	removedPath, err := filepath.EvalSymlinks(removed.Path)
	if err != nil {
		return fmt.Errorf("resolve plugin cache entry: %w", err)
	}
	rel, err := filepath.Rel(cacheRoot, removedPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refuse to remove plugin outside cache: %s", removed.Path)
	}
	if err := os.RemoveAll(removedPath); err != nil {
		return fmt.Errorf("remove plugin cache: %w", err)
	}
	registry.Plugins = remaining
	return SaveInstalledRegistryForSkillsDir(skillsDir, registry)
}

func copyPluginTree(source, destination string) error {
	return filepath.WalkDir(source, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin bundle symlink rejected: %s", name)
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("plugin bundle non-regular file rejected: %s", name)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func extractPluginZip(filename, destination string) error {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return fmt.Errorf("open plugin zip: %w", err)
	}
	defer reader.Close()
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for _, file := range reader.File {
		name := filepath.ToSlash(file.Name)
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe zip path %q", file.Name)
		}
		clean := filepath.ToSlash(filepath.Clean(name))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("zip path escapes plugin root: %q", file.Name)
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("zip symlink rejected: %q", file.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			src.Close()
			return err
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func digestTree(root string) (string, error) {
	var entries []FileEntryV1
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected in plugin bundle: %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular plugin file rejected: %s", name)
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		entries = append(entries, FileEntryV1{Path: filepath.ToSlash(rel), SHA256: hashBytes(data)})
		return nil
	})
	if err != nil {
		return "", err
	}
	return digestEntries(entries), nil
}

type FileEntryV1 struct {
	Path   string
	SHA256 string
}

func digestEntries(entries []FileEntryV1) string {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	h := sha256.New()
	h.Write([]byte("codeagent-plugin-bundle-v1\x00"))
	for _, entry := range entries {
		h.Write([]byte(entry.Path))
		h.Write([]byte{0})
		h.Write([]byte(entry.SHA256))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
