package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/systemskills"
)

func TestCodeagentdCleanInstallSystemSkills(t *testing.T) {
	home := t.TempDir()
	if err := systemskills.InstallForHome(home, systemskills.Embedded(), "test:codeagentd"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"review-change", "verify-change", "skill-creator", "ui-adaptation"} {
		if _, err := os.Stat(filepath.Join(home, ".codeagent", "skills", ".system", name, "SKILL.md")); err != nil {
			t.Fatalf("system skill %q: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codeagent", "skills", ".system", systemskills.MarkerFilename)); err != nil {
		t.Fatal(err)
	}
}

func TestCodeagentdLegacyFlatBundleUpgrade(t *testing.T) {
	home := t.TempDir()
	skillsRoot := filepath.Join(home, ".codeagent", "skills")
	for _, name := range []string{"review-change", "verify-change", "skill-creator"} {
		sub, err := fs.Sub(systemskills.Embedded(), name)
		if err != nil {
			t.Fatal(err)
		}
		copyCodeagentdTestTree(t, sub, filepath.Join(skillsRoot, name))
	}
	if err := systemskills.InstallForHome(home, systemskills.Embedded(), "test:codeagentd-upgrade"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"review-change", "verify-change", "skill-creator"} {
		if _, err := os.Stat(filepath.Join(skillsRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy flattened skill %q was not migrated", name)
		}
		if _, err := os.Stat(filepath.Join(skillsRoot, ".system", name, "SKILL.md")); err != nil {
			t.Fatalf("system skill %q: %v", name, err)
		}
	}
}

func copyCodeagentdTestTree(t *testing.T, source fs.FS, destination string) {
	t.Helper()
	if err := fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
}
