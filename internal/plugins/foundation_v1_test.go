package plugins

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTestPlugin(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".codeagent-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "skills", "search"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"schema":      PluginSchemaV1,
		"name":        "docs",
		"version":     "1.0.0",
		"description": "Docs plugin",
		"skills":      []string{"./skills"},
		"mcpServers": map[string]any{
			"lookup": map[string]any{"command": "docs-mcp"},
		},
		"apps":      []string{"docs"},
		"hooks":     []string{},
		"interface": map[string]any{"displayName": "Docs"},
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

func TestInstallLocalNamespaceDisableAndTamper(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	source := filepath.Join(t.TempDir(), "docs-plugin")
	writeTestPlugin(t, source)
	if _, err := InstallLocal(skillsDir, source, InstallOptions{Source: source}); err != ErrConfirmationRequired {
		t.Fatalf("unconfirmed install error = %v", err)
	}
	entry, err := InstallLocal(skillsDir, source, InstallOptions{Confirmed: true, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path == "" || entry.BundleSHA256 == "" || entry.ManifestSHA256 == "" {
		t.Fatalf("installed entry incomplete: %#v", entry)
	}
	roots, err := SkillRootsForSkillsDir(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].PluginName != "docs" {
		t.Fatalf("roots = %#v", roots)
	}
	cfg, err := MCPConfigForSkillsDir(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "docs__lookup" {
		t.Fatalf("MCP servers = %#v", cfg.Servers)
	}
	if err := SetPluginEnabled(skillsDir, "docs", false); err != nil {
		t.Fatal(err)
	}
	roots, err = SkillRootsForSkillsDir(skillsDir)
	if err != nil || len(roots) != 0 {
		t.Fatalf("disabled roots = %#v, err=%v", roots, err)
	}
	cfg, err = MCPConfigForSkillsDir(skillsDir)
	if err != nil || len(cfg.Servers) != 0 {
		t.Fatalf("disabled MCP = %#v, err=%v", cfg.Servers, err)
	}
	if err := SetPluginEnabled(skillsDir, "docs", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry.Path, "skills", "search", "SKILL.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SkillRootsForSkillsDir(skillsDir); err == nil {
		t.Fatal("tampered plugin bundle was accepted")
	}
}

func TestInstallLocalZip(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	source := filepath.Join(t.TempDir(), "docs-plugin")
	writeTestPlugin(t, source)
	zipPath := filepath.Join(t.TempDir(), "docs.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	_ = filepath.WalkDir(source, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(source, name)
		info, _ := entry.Info()
		header, _ := zip.FileInfoHeader(info)
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		dst, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		_, err = dst.Write(data)
		return err
	})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallLocal(skillsDir, zipPath, InstallOptions{Confirmed: true, Source: zipPath}); err != nil {
		t.Fatalf("InstallLocal(zip): %v", err)
	}
}

func TestInstallLocalRejectsZipTraversalAndSymlink(t *testing.T) {
	for name, makeEntry := range map[string]func(*zip.Writer) error{
		"traversal": func(writer *zip.Writer) error {
			part, err := writer.Create("../escape.txt")
			if err != nil {
				return err
			}
			_, err = part.Write([]byte("escape"))
			return err
		},
		"symlink": func(writer *zip.Writer) error {
			header := &zip.FileHeader{Name: "link", Method: zip.Store}
			header.SetMode(0o777 | os.ModeSymlink)
			part, err := writer.CreateHeader(header)
			if err != nil {
				return err
			}
			_, err = part.Write([]byte("../outside"))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			zipPath := filepath.Join(t.TempDir(), "bad.zip")
			file, err := os.Create(zipPath)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(file)
			if err := makeEntry(writer); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			skillsDir := filepath.Join(t.TempDir(), "skills")
			if _, err := InstallLocal(skillsDir, zipPath, InstallOptions{Confirmed: true, Source: zipPath}); err == nil {
				t.Fatal("unsafe zip was accepted")
			}
		})
	}
}

func TestUninstallLocalRejectsCacheSymlinkEscape(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	source := filepath.Join(t.TempDir(), "docs-plugin")
	writeTestPlugin(t, source)
	entry, err := InstallLocal(skillsDir, source, InstallOptions{Confirmed: true, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(entry.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, entry.Path); err != nil {
		t.Fatal(err)
	}
	if err := UninstallLocal(skillsDir, "docs"); err == nil {
		t.Fatal("uninstall followed a cache symlink outside the plugin cache")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory was removed: %v", err)
	}
	registry, err := LoadInstalledRegistryForSkillsDir(skillsDir)
	if err != nil || len(registry.Plugins) != 1 {
		t.Fatalf("registry after refused uninstall = %#v, %v", registry, err)
	}
}

func TestParseInterfaceMetadataAllowlistsSchemaFields(t *testing.T) {
	raw := []byte(`{
		"displayName":"Docs Center",
		"category":"documentation",
		"capabilities":["search","summarize"],
		"composerIcon":"https://cdn.example.com/icon.svg",
		"logo":"assets/logo.png",
		"brandColor":"#112233",
		"websiteURL":"https://example.com/docs",
		"command":"must-not-leak",
		"env":{"TOKEN":"must-not-leak"}
	}`)
	meta, ok := ParseInterfaceMetadata(raw)
	if !ok {
		t.Fatal("interface metadata was not parsed")
	}
	if meta.DisplayName != "Docs Center" || meta.Category != "documentation" ||
		meta.Icon != "https://cdn.example.com/icon.svg" || meta.Logo != "assets/logo.png" ||
		meta.BrandColor != "#112233" || meta.WebsiteURL != "https://example.com/docs" {
		t.Fatalf("metadata = %#v", meta)
	}
	if len(meta.Capabilities) != 2 || meta.Capabilities[0] != "search" || meta.Capabilities[1] != "summarize" {
		t.Fatalf("capabilities = %#v", meta.Capabilities)
	}
}
