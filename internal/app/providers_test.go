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
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
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
	if plus.Credential != (settings.CredentialRef{Namespace: "llm", Name: "qwen"}) {
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
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
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
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
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
	_, err := LoadSettingsBytes([]byte(`
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
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
	key := aliasKey("openrouter", "anthropic/claude-sonnet-4")
	mc := cfg.Models[key]
	if mc.Credential != (settings.CredentialRef{Namespace: "llm", Name: "openrouter"}) {
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
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}

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

// Reasoning capability fields (reasoning_effort / supported_reasoning_efforts /
// can_disable_reasoning) survive the grouped → flat expansion into ModelConfig
// and its catalog metadata — the data the client's effort picker renders from.
func TestFromSettingsProviderReasoningFields(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "custom-svc": {
      "base_url": "https://api.custom.example/v1",
      "api": "openai",
      "models": [
        {
          "id": "reasoner-x",
          "reasoning_effort": "high",
          "supported_reasoning_efforts": ["low", "medium", "high", "x-high", "max"],
          "can_disable_reasoning": false
        }
      ]
    }
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models[aliasKey("custom-svc", "reasoner-x")]
	if mc.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q, want high (request-layer default)", mc.ReasoningEffort)
	}
	if got := mc.Catalog.SupportedReasoningEfforts; len(got) != 5 || got[3] != "x-high" || got[4] != "max" {
		t.Errorf("supported_reasoning_efforts = %v, want low..max incl. x-high", got)
	}
	if mc.Catalog.CanDisableReasoning == nil || *mc.Catalog.CanDisableReasoning {
		t.Errorf("can_disable_reasoning = %v, want explicit false (reasoner-only)", mc.Catalog.CanDisableReasoning)
	}
}

// The built-in registry aligns grouped-provider models with the provider's
// official reasoning capability: a model id known to the registry gets its
// supports_reasoning / supported_reasoning_efforts / can_disable_reasoning
// defaults filled when the config leaves them unset, and explicit config
// values are never overwritten.
func TestFromSettingsRegistryFillsReasoningCapability(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "deepseek": {
      "base_url": "https://api.deepseek.com",
      "api": "openai",
      "models": [
        {"id": "deepseek-v4-flash"},
        {"id": "deepseek-v4-pro", "can_disable_reasoning": false}
      ]
    }
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
	flash := cfg.Models[aliasKey("deepseek", "deepseek-v4-flash")]
	if flash.Catalog.SupportsReasoning == nil || !*flash.Catalog.SupportsReasoning {
		t.Errorf("supports_reasoning = %v, want registry-filled true", flash.Catalog.SupportsReasoning)
	}
	if got := flash.Catalog.SupportedReasoningEfforts; len(got) != 3 || got[0] != "low" || got[2] != "high" {
		t.Errorf("supported_reasoning_efforts = %v, want registry [low medium high]", got)
	}
	if flash.Catalog.CanDisableReasoning == nil || !*flash.Catalog.CanDisableReasoning {
		t.Errorf("can_disable_reasoning = %v, want registry true", flash.Catalog.CanDisableReasoning)
	}
	// Explicit config wins over the registry.
	pro := cfg.Models[aliasKey("deepseek", "deepseek-v4-pro")]
	if pro.Catalog.CanDisableReasoning == nil || *pro.Catalog.CanDisableReasoning {
		t.Errorf("can_disable_reasoning = %v, want explicit false preserved", pro.Catalog.CanDisableReasoning)
	}
	// An unknown model id under a known connection gets no invented capability.
	unknown := cfg.Models[aliasKey("deepseek", "deepseek-chat")]
	if unknown.Catalog.SupportsReasoning != nil || len(unknown.Catalog.SupportedReasoningEfforts) != 0 {
		t.Errorf("unknown model capability = %+v, want unset", unknown.Catalog)
	}
}

// BuiltinProviderModelIDs backs the api-key-only onboarding flow: known
// connections expose their suggested model ids, unknown ids are rejected, and
// connections without suggested models (ollama) return an empty-but-ok list.
func TestBuiltinProviderModelIDs(t *testing.T) {
	ids, ok := BuiltinProviderModelIDs("deepseek")
	if !ok || len(ids) != 3 || ids[0] != "deepseek-v4-flash" {
		t.Errorf("BuiltinProviderModelIDs(deepseek) = %v, %v; want 3 ids", ids, ok)
	}
	if ids, ok := BuiltinProviderModelIDs("ollama"); !ok || len(ids) != 0 {
		t.Errorf("BuiltinProviderModelIDs(ollama) = %v, %v; want empty but ok", ids, ok)
	}
	if ids, ok := BuiltinProviderModelIDs("custom-svc"); ok || ids != nil {
		t.Errorf("BuiltinProviderModelIDs(custom-svc) = %v, %v; want unknown", ids, ok)
	}
}

// An api-key-only built-in connection (persisted WITHOUT a models list)
// expands into a fully-capable model space: FromSettings merges the registry's
// model ids at expansion time, and the registry fills instance data +
// capabilities — settings.json stays snapshot-free, so a provider shipping new
// models reaches existing settings files with zero changes.
func TestFromSettingsBuiltinProviderAPIKeyOnlyFlow(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "deepseek": {"credential": {"namespace": "llm", "name": "deepseek"}}
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Providers["deepseek"].Models) != 0 {
		t.Fatal("test setup: models must be absent on disk")
	}
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
	flash := cfg.Models[aliasKey("deepseek", "deepseek-v4-flash")]
	if flash.Model != "deepseek-v4-flash" {
		t.Fatalf("model = %q", flash.Model)
	}
	if flash.ContextWindow != 1_000_000 {
		t.Errorf("context_window = %d, want registry 1_000_000", flash.ContextWindow)
	}
	if !flash.WebSearch {
		t.Error("web_search = false, want registry true")
	}
	if flash.Catalog.SupportsReasoning == nil || !*flash.Catalog.SupportsReasoning {
		t.Error("supports_reasoning = nil/false, want registry true")
	}
	if got := flash.Catalog.SupportedReasoningEfforts; len(got) != 3 {
		t.Errorf("supported_reasoning_efforts = %v, want [low medium high]", got)
	}
	// Registry per-model pricing applies where declared.
	pro := cfg.Models[aliasKey("deepseek", "deepseek-v4-pro")]
	if pro.InputPricePerM != 0.45 {
		t.Errorf("input_price_per_million = %v, want registry 0.45", pro.InputPricePerM)
	}
	// A custom provider without models still expands to nothing.
	sf.Providers["custom-svc"] = settings.ServiceConfig{BaseURL: "https://x.example"}
	cfg2, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg2.Models[aliasKey("custom-svc", "anything")]; ok {
		t.Error("custom provider without models must expand to no models")
	}
}

func keysOf(m map[string]settings.ModelConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
