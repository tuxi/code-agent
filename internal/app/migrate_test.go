package app

import (
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/settings"
)

// MigrateConfigToSettings round-trip: user + project config.yaml merge into
// settings.json, and settings.Load reads it back with the same values.
func TestMigrateConfigToSettings(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)

	// User-level config.yaml.
	userDir := filepath.Join(home, ".codeagent")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userYAML := `
default_model: deepseek
models:
  deepseek:
    model: deepseek-v4-flash
    credential: {namespace: llm, name: deepseek}
credentials:
  llm:
    deepseek: {source: env, env: DEEPSEEK_API_KEY}
agent:
  max_steps: 68
`
	if err := os.WriteFile(filepath.Join(userDir, "config.yaml"), []byte(userYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project-level config.yaml.
	projDir := filepath.Join(root, ".codeagent")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projYAML := `
default_model: deepseek-pro
models:
  deepseek-pro:
    model: deepseek-v4-pro
    credential: {namespace: llm, name: deepseek}
`
	if err := os.WriteFile(filepath.Join(projDir, "config.yaml"), []byte(projYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MigrateConfigToSettings(root, home); err != nil {
		t.Fatalf("MigrateConfigToSettings: %v", err)
	}

	// Read back via settings.Load.
	set := settings.Load(root, home, os.Stderr)
	if set.DefaultModel != "deepseek-pro" {
		t.Errorf("default_model = %q, want deepseek-pro (project wins)", set.DefaultModel)
	}
	if set.Agent.MaxSteps != 68 {
		t.Errorf("max_steps = %d, want 68 (user agent inherited)", set.Agent.MaxSteps)
	}
	if _, ok := set.Models["deepseek"]; !ok {
		t.Errorf("user model deepseek missing: %v", set.Models)
	}
	if _, ok := set.Models["deepseek-pro"]; !ok {
		t.Errorf("project model deepseek-pro missing: %v", set.Models)
	}
	if cc, ok := set.Credentials["llm"]["deepseek"]; !ok || cc.Source != "env" {
		t.Errorf("credential llm/deepseek = %+v, want {env}", cc)
	}
}

// Migration of an empty config.yaml is a no-op (no error, no file).
func TestMigrateConfigToSettingsEmpty(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)

	if err := MigrateConfigToSettings(root, home); err != nil {
		t.Fatalf("MigrateConfigToSettings(empty): %v", err)
	}
	// No user settings.json should have been written (nothing to migrate).
	if _, err := os.Stat(filepath.Join(home, ".codeagent", "settings.json")); err == nil {
		t.Error("expected no settings.json for empty migration")
	}
}
