package app

import (
	"strings"
	"testing"

	"code-agent/internal/settings"
)

// TestLoadSettingsBytesYAMLFlatModels guards the YAML parse path: embedded hosts
// and legacy config.yaml-shaped documents use snake_case keys, which must map
// onto the json-tagged settings structs (base_url → BaseURL etc.). A regression
// here silently drops every model field.
func TestLoadSettingsBytesYAMLFlatModels(t *testing.T) {
	cfg, err := LoadSettingsBytes([]byte(`
default_model: deepseek
credentials:
  llm:
    deepseek:
      source: env
      env: DEEPSEEK_API_KEY
models:
  deepseek:
    provider: openai
    base_url: https://api.deepseek.com
    model: deepseek-v4-flash
    credential:
      namespace: llm
      name: deepseek
agent:
  max_steps: 16
  compact_ratio: 0.8
web:
  search:
    provider: searxng
    top_k: 3
`))
	if err != nil {
		t.Fatalf("LoadSettingsBytes YAML: %v", err)
	}
	mc := cfg.Models["deepseek"]
	if mc.BaseURL != "https://api.deepseek.com" {
		t.Errorf("BaseURL = %q, want https://api.deepseek.com", mc.BaseURL)
	}
	if mc.Model != "deepseek-v4-flash" {
		t.Errorf("Model = %q, want deepseek-v4-flash", mc.Model)
	}
	if mc.Credential != (settings.CredentialRef{Namespace: "llm", Name: "deepseek"}) {
		t.Errorf("Credential = %+v, want llm/deepseek", mc.Credential)
	}
	if cfg.DefaultModel != "deepseek" {
		t.Errorf("DefaultModel = %q, want deepseek", cfg.DefaultModel)
	}
	if cfg.Agent.MaxSteps != 16 {
		t.Errorf("MaxSteps = %d, want 16", cfg.Agent.MaxSteps)
	}
	if cfg.Agent.CompactRatio != 0.8 {
		t.Errorf("CompactRatio = %v, want 0.8", cfg.Agent.CompactRatio)
	}
	if cc := cfg.Credentials["llm"]["deepseek"]; cc.Source != "env" || cc.Env != "DEEPSEEK_API_KEY" {
		t.Errorf("credential config = %+v, want env/DEEPSEEK_API_KEY", cc)
	}
	if cfg.Web.Search.Provider != "searxng" || cfg.Web.Search.TopK != 3 {
		t.Errorf("web.search = %+v, want searxng/top_k=3", cfg.Web.Search)
	}
}

// TestLoadSettingsBytesYAMLProviders guards grouped providers expansion from a
// YAML document (the settings.json shape authored by hand or by an embedded host).
func TestLoadSettingsBytesYAMLProviders(t *testing.T) {
	cfg, err := LoadSettingsBytes([]byte(`
providers:
  qwen:
    base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
    api: openai
    models:
      - id: qwen3-coder-plus
        context_window: 128000
`))
	if err != nil {
		t.Fatalf("LoadSettingsBytes YAML providers: %v", err)
	}
	key := aliasKey("qwen", "qwen3-coder-plus")
	mc, ok := cfg.Models[key]
	if !ok {
		t.Fatalf("alias model %q missing; models=%v", key, keysOf(cfg.Models))
	}
	if mc.BaseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Errorf("BaseURL = %q, want provider base_url", mc.BaseURL)
	}
	if mc.ContextWindow != 128000 {
		t.Errorf("ContextWindow = %d, want 128000", mc.ContextWindow)
	}
	if _, ok := cfg.Models["qwen/qwen3-coder-plus"]; !ok {
		t.Error("friendly key qwen/qwen3-coder-plus missing")
	}
}

// TestLoadSettingsBytesJSON guards the JSON path (settings.json on disk).
func TestLoadSettingsBytesJSON(t *testing.T) {
	cfg, err := LoadSettingsBytes([]byte(`{
		"default_model": "deepseek",
		"credentials": {"llm": {"deepseek": {"source": "env", "env": "DEEPSEEK_API_KEY"}}},
		"models": {"deepseek": {"provider": "openai", "base_url": "https://api.deepseek.com", "model": "deepseek-v4-flash"}}
	}`))
	if err != nil {
		t.Fatalf("LoadSettingsBytes JSON: %v", err)
	}
	mc := cfg.Models["deepseek"]
	if mc.BaseURL != "https://api.deepseek.com" || cfg.DefaultModel != "deepseek" {
		t.Errorf("JSON parse: BaseURL=%q DefaultModel=%q", mc.BaseURL, cfg.DefaultModel)
	}
}

// TestLoadSettingsBytesExplicitEmptyModels guards the zero-model read-only host
// mode: an explicit models: {} or providers: {} must NOT be replaced by the
// built-in deepseek default.
func TestLoadSettingsBytesExplicitEmptyModels(t *testing.T) {
	for _, doc := range []string{"models: {}", "providers: {}"} {
		cfg, err := LoadSettingsBytes([]byte(doc))
		if err != nil {
			t.Fatalf("LoadSettingsBytes(%q): %v", doc, err)
		}
		if len(cfg.Models) != 0 {
			t.Errorf("LoadSettingsBytes(%q) models = %d, want 0", doc, len(cfg.Models))
		}
	}
}

// TestSelectModelRegistryFallback guards R2.3: a known connection name that is
// not declared in config still resolves through the built-in registry.
func TestSelectModelRegistryFallback(t *testing.T) {
	cfg, err := LoadSettingsBytes(nil)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := SelectModel("glm", cfg)
	if err != nil {
		t.Fatalf("SelectModel(glm): %v", err)
	}
	if mc.BaseURL != "https://open.bigmodel.cn/api/paas/v4" {
		t.Errorf("BaseURL = %q, want registry glm base URL", mc.BaseURL)
	}
	if mc.Credential != (settings.CredentialRef{Namespace: "llm", Name: "glm"}) {
		t.Errorf("Credential = %+v, want llm/glm", mc.Credential)
	}
}

// TestSelectModelUnknownListsModels guards the error message naming the
// configured models so a typo is self-explanatory.
func TestSelectModelUnknownListsModels(t *testing.T) {
	cfg, err := LoadSettingsBytes(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SelectModel("no-such-model", cfg); err == nil || !strings.Contains(err.Error(), "configured models") {
		t.Errorf("unknown model error = %v, want configured-models list", err)
	}
}
