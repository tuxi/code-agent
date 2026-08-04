package app

import (
	"os"
	"path/filepath"
	"testing"
)

// R2.1: a model config that omits base_url/api_key_env gets them from the
// built-in registry; explicit values are never overwritten.
func TestRegistryFillsDefaults(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
default_model: deepseek
models:
  deepseek:
    model: deepseek-v4-pro
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["deepseek"]
	if mc.BaseURL != "https://api.deepseek.com" {
		t.Errorf("base_url = %q, want registry default", mc.BaseURL)
	}
	if mc.APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Errorf("api_key_env = %q, want registry default", mc.APIKeyEnv)
	}
}

func TestRegistryDoesNotOverrideExplicit(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  deepseek:
    base_url: https://proxy.example.com/v1
    api_key_env: MY_CUSTOM_KEY
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["deepseek"]
	if mc.BaseURL != "https://proxy.example.com/v1" {
		t.Errorf("base_url = %q, want explicit value preserved", mc.BaseURL)
	}
	if mc.APIKeyEnv != "MY_CUSTOM_KEY" {
		t.Errorf("api_key_env = %q, want explicit value preserved", mc.APIKeyEnv)
	}
}

// R2.3: a known connection name that was never declared in the config still
// resolves via the registry fallback; unknown names still error.
func TestSelectModelRegistryFallback(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "sk-registry")

	// Empty config (no file) → no models declared for qwen (only the built-in
	// deepseek default exists), but a known connection name must still resolve
	// via the registry.
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	mc, err := cfg.SelectModel("qwen")
	if err != nil {
		t.Fatalf("SelectModel(qwen) via registry: %v", err)
	}
	if mc.BaseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" || mc.Model != "qwen3-coder-plus" {
		t.Errorf("registry fallback model = %+v", mc)
	}

	// Unknown names still fail.
	if _, err := cfg.SelectModel("gpt"); err == nil {
		t.Error("SelectModel(gpt) must fail (not in registry)")
	}
}

// R2.2: layered loading merges a user-global config with a project config;
// project wins on conflict, user models/credentials survive when the project
// does not redeclare them, and a missing user file is not an error.
func TestLoadConfigLayered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv("DEEPSEEK_API_KEY")

	// User-global config: declares qwen + a deepseek model with a credential.
	userDir := filepath.Join(home, ".codeagent")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(userDir, "config.yaml")
	userYAML := `
default_model: qwen
models:
  qwen:
    model: qwen3-coder-plus
    credential: {namespace: llm, name: qwen}
  deepseek:
    model: deepseek-v4-pro
credentials:
  llm:
    qwen: {source: env, env: DASHSCOPE_API_KEY}
`
	if err := os.WriteFile(userPath, []byte(userYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project config: redeclares deepseek (project wins), sets its own
	// default_model, no user-level key data.
	projDir := t.TempDir()
	projPath := filepath.Join(projDir, "config.yaml")
	projYAML := `
default_model: deepseek
models:
  deepseek:
    model: deepseek-v4-flash
`
	if err := os.WriteFile(projPath, []byte(projYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigLayered(projPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModel != "deepseek" {
		t.Errorf("default_model = %q, want project's deepseek", cfg.DefaultModel)
	}
	// Project redeclared deepseek → project's model wins.
	if mc := cfg.Models["deepseek"]; mc.Model != "deepseek-v4-flash" {
		t.Errorf("deepseek model = %q, want project's deepseek-v4-flash", mc.Model)
	}
	// User's qwen survives the merge.
	if mc, ok := cfg.Models["qwen"]; !ok || mc.Model != "qwen3-coder-plus" {
		t.Errorf("qwen = %+v, want merged from user config", mc)
	}
	// User's credentials survive.
	if cc, ok := cfg.Credentials["llm"]["qwen"]; !ok || cc.Source != "env" {
		t.Errorf("llm/qwen credential = %+v, want merged from user config", cc)
	}
}

// A missing user config is not an error — layered loading degrades to the
// project layer alone.
func TestLoadConfigLayeredMissingUserConfig(t *testing.T) {
	home := t.TempDir() // no .codeagent/config.yaml inside
	t.Setenv("HOME", home)

	projPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(projPath, []byte("default_model: deepseek\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigLayered(projPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModel != "deepseek" {
		t.Errorf("default_model = %q", cfg.DefaultModel)
	}
}

// R2/T1.3: AvailableModelNames offers the config-declared models plus the
// built-in registry connections, deduplicated.
func TestAvailableModelNamesIncludesRegistry(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  deepseek:
    model: deepseek-v4-pro
`))
	if err != nil {
		t.Fatal(err)
	}
	names := cfg.AvailableModelNames()
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		seen[n] = true
	}
	// deepseek is both declared and in the registry — must appear once.
	if !seen["deepseek"] {
		t.Errorf("AvailableModelNames missing deepseek: %v", names)
	}
	// Registry-only connections appear (open-box).
	for _, want := range []string{"qwen", "glm", "ollama", "gateway"} {
		if !seen[want] {
			t.Errorf("AvailableModelNames missing registry connection %q: %v", want, names)
		}
	}
	// Sorted.
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("AvailableModelNames not sorted: %v", names)
		}
	}
}
