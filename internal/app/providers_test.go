package app

import (
	"strings"
	"testing"

	"code-agent/internal/settings"
)

// design-providers-grouped-config.md §3.1/§3.3: a grouped provider expands to
// flat ModelConfig entries keyed by the alias (provider.<b64>.model.<b64>),
// inheriting service-level fields with per-model differences.
func TestFromSettingsProvidersExpand(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "qwen": {
      "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
      "api": "openai",
      "credential": {"namespace": "llm", "name": "qwen"},
      "models": [
        {"id": "qwen3-coder-plus"},
        {"id": "qwen3.7-max", "context_window": 256000}
      ]
    }
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := FromSettings(settings.Settings{Providers: sf.Providers})
	// Both models expanded with inherited fields.
	plusKey := aliasKey("qwen", "qwen3-coder-plus")
	maxKey := aliasKey("qwen", "qwen3.7-max")
	plus, ok := cfg.Models[plusKey]
	if !ok {
		t.Fatalf("model %q missing; keys=%v", plusKey, keysOf(cfg.Models))
	}
	if plus.BaseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Errorf("base_url = %q, want inherited from provider", plus.BaseURL)
	}
	if plus.Credential != (CredentialRef{Namespace: "llm", Name: "qwen"}) {
		t.Errorf("credential = %+v, want inherited llm/qwen", plus.Credential)
	}
	if plus.Model != "qwen3-coder-plus" {
		t.Errorf("model = %q, want qwen3-coder-plus", plus.Model)
	}
	if plus.Catalog.ConnectionID != "qwen" || plus.Catalog.ProviderID != "qwen" {
		t.Errorf("catalog = %+v, want connection/provider qwen", plus.Catalog)
	}
	// Per-model difference override.
	max := cfg.Models[maxKey]
	if max.ContextWindow != 256000 {
		t.Errorf("qwen3.7-max context_window = %d, want 256000 (override)", max.ContextWindow)
	}
}

// Cross-provider same model id coexists (deepseek-v4-flash under dashscope AND
// openrouter) — distinct alias keys, no collision.
func TestFromSettingsCrossProviderSameModel(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "dashscope": {"base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1", "models": [{"id": "deepseek-v4-flash"}]},
    "openrouter": {"base_url": "https://openrouter.ai/api/v1", "models": [{"id": "deepseek-v4-flash"}]}
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := FromSettings(settings.Settings{Providers: sf.Providers})
	dsKey := aliasKey("dashscope", "deepseek-v4-flash")
	orKey := aliasKey("openrouter", "deepseek-v4-flash")
	if dsKey == orKey {
		t.Fatal("alias keys must differ across providers")
	}
	if _, ok := cfg.Models[dsKey]; !ok {
		t.Errorf("dashscope/deepseek-v4-flash missing")
	}
	if _, ok := cfg.Models[orKey]; !ok {
		t.Errorf("openrouter/deepseek-v4-flash missing")
	}
}

// OpenRouter model ids contain "/" — the alias key must keep them slash-free
// and the expanded model must carry the full wire id.
func TestFromSettingsOpenRouterSlashModelID(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "openrouter": {"base_url": "https://openrouter.ai/api/v1", "api": "openai", "models": [{"id": "deepseek/deepseek-chat"}]}
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := FromSettings(settings.Settings{Providers: sf.Providers})
	key := aliasKey("openrouter", "deepseek/deepseek-chat")
	if strings.Contains(key, "/") {
		t.Errorf("alias key %q must not contain /", key)
	}
	mc, ok := cfg.Models[key]
	if !ok {
		t.Fatalf("model missing; keys=%v", keysOf(cfg.Models))
	}
	if mc.Model != "deepseek/deepseek-chat" {
		t.Errorf("wire model = %q, want full slash id", mc.Model)
	}
}

// design-providers-grouped-config.md §3.1: a flat models key with "/" is
// rejected (it would collide with grouped expansion).
func TestFlatModelKeyRejectsSlash(t *testing.T) {
	_, err := LoadConfigBytes([]byte(`
models:
  dashscope/qwen3-coder-plus:
    provider: openai
    base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
    model: qwen3-coder-plus
`))
	if err == nil {
		t.Fatal("expected error for flat models key with /")
	}
	if !strings.Contains(err.Error(), "must not contain") {
		t.Errorf("error = %v, want flat-key rejection message", err)
	}
}

// Default credential derivation: a provider without explicit credential gets
// llm/<provider-id>.
func TestFromSettingsProviderDefaultCredential(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "openrouter": {"base_url": "https://openrouter.ai/api/v1", "models": [{"id": "anthropic/claude-sonnet-4"}]}
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := FromSettings(settings.Settings{Providers: sf.Providers})
	key := aliasKey("openrouter", "anthropic/claude-sonnet-4")
	mc := cfg.Models[key]
	if mc.Credential != (CredentialRef{Namespace: "llm", Name: "openrouter"}) {
		t.Errorf("credential = %+v, want default llm/openrouter", mc.Credential)
	}
}

// Per-model API override: a model under a provider can declare its own api type,
// overriding the service-level default. Used by OpenCode Go where some models
// speak the responses protocol while the service default is openai-compatible.
func TestFromSettingsPerModelAPIOverride(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "opencode-go": {
      "base_url": "https://opencode.ai/zen/go/v1",
      "api": "openai",
      "credential": {"namespace": "llm", "name": "opencode-go"},
      "models": [
        {"id": "deepseek-v4-flash"},
        {"id": "gpt-5.6-luna", "api": "responses"}
      ]
    }
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := FromSettings(settings.Settings{Providers: sf.Providers})

	flashKey := aliasKey("opencode-go", "deepseek-v4-flash")
	lunaKey := aliasKey("opencode-go", "gpt-5.6-luna")

	flash, ok := cfg.Models[flashKey]
	if !ok {
		t.Fatalf("model %q missing; keys=%v", flashKey, keysOf(cfg.Models))
	}
	if flash.Provider != "openai" {
		t.Errorf("deepseek-v4-flash provider = %q, want inherited openai", flash.Provider)
	}

	luna, ok := cfg.Models[lunaKey]
	if !ok {
		t.Fatalf("model %q missing; keys=%v", lunaKey, keysOf(cfg.Models))
	}
	if luna.Provider != "responses" {
		t.Errorf("gpt-5.6-luna provider = %q, want per-model override responses", luna.Provider)
	}
	if luna.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Errorf("gpt-5.6-luna base_url = %q, want inherited from provider", luna.BaseURL)
	}
}

func keysOf(m map[string]ModelConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

