package app

import (
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

// The gateway connection is a declaration (no endpoint, no env key) whose
// credential is resolved at call time from the injected resolver. Selecting it
// as the default model must not fail the static credential check.
func TestSelectModelGatewayRegistryFallbackWithoutCredential(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	mc, err := cfg.SelectModel("gateway")
	if err != nil {
		t.Fatalf("SelectModel(gateway) via registry must not require a static credential: %v", err)
	}
	if mc.Name != "gateway" {
		t.Errorf("SelectModel(gateway) = %+v", mc)
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
	for _, want := range []string{"qwen", "glm", "ollama", "gateway", "opencode-go"} {
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

// design-provider-id-model.md: a known service id ("provider: deepseek") resolves
// to its api type, fills base_url/env, and records the service id in
// Catalog.ProviderID.
func TestRegistryServiceIDResolvesToAPIType(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  deepseek:
    provider: deepseek
    model: deepseek-v4-flash
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["deepseek"]
	if mc.Provider != "openai" {
		t.Errorf("provider = %q, want resolved api type openai", mc.Provider)
	}
	if mc.BaseURL != "https://api.deepseek.com" {
		t.Errorf("base_url = %q, want registry default", mc.BaseURL)
	}
	if mc.APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Errorf("api_key_env = %q, want registry default", mc.APIKeyEnv)
	}
	if mc.Catalog.ProviderID != "deepseek" {
		t.Errorf("catalog.provider_id = %q, want deepseek", mc.Catalog.ProviderID)
	}
}

// A generic api type ("provider: openai" + explicit base_url) is NOT treated as
// a service id — the explicit values are preserved (backward compat).
func TestRegistryGenericAPITypeNotOverridden(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  deepseek:
    provider: openai
    base_url: https://proxy.example.com/v1
    model: deepseek-v4-flash
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["deepseek"]
	if mc.Provider != "openai" {
		t.Errorf("provider = %q, want explicit openai preserved", mc.Provider)
	}
	if mc.BaseURL != "https://proxy.example.com/v1" {
		t.Errorf("base_url = %q, want explicit value preserved", mc.BaseURL)
	}
}

// openrouter is a known service: base_url/env/api type resolve; model ids may
// contain a "/" (e.g. deepseek/deepseek-chat).
func TestRegistryOpenRouterService(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  openrouter:
    provider: openrouter
    model: deepseek/deepseek-chat
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["openrouter"]
	if mc.Provider != "openai" {
		t.Errorf("provider = %q, want resolved openai", mc.Provider)
	}
	if mc.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("base_url = %q, want openrouter endpoint", mc.BaseURL)
	}
	if mc.APIKeyEnv != "OPENROUTER_API_KEY" {
		t.Errorf("api_key_env = %q, want OPENROUTER_API_KEY", mc.APIKeyEnv)
	}
	if mc.Model != "deepseek/deepseek-chat" {
		t.Errorf("model = %q, want deepseek/deepseek-chat (slash preserved)", mc.Model)
	}
}

// gateway: provider resolves to openai, but base_url/env stay empty (host injects).
func TestRegistryGatewayService(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  gateway:
    provider: gateway
    model: ""
    credential: {namespace: gateway, name: default}
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["gateway"]
	if mc.Provider != "openai" {
		t.Errorf("provider = %q, want resolved openai", mc.Provider)
	}
	if mc.BaseURL != "" {
		t.Errorf("base_url = %q, want empty (host injected)", mc.BaseURL)
	}
}

// opencode-go: known service with base_url/env, default model deepseek-v4-flash.
func TestRegistryOpenCodeGoService(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  opencode-go:
    model: deepseek-v4-pro
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["opencode-go"]
	if mc.Provider != "openai" {
		t.Errorf("provider = %q, want resolved openai", mc.Provider)
	}
	if mc.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Errorf("base_url = %q, want opencode-go endpoint", mc.BaseURL)
	}
	if mc.APIKeyEnv != "OPENCODE_GO_API_KEY" {
		t.Errorf("api_key_env = %q, want OPENCODE_GO_API_KEY", mc.APIKeyEnv)
	}
	if mc.Model != "deepseek-v4-pro" {
		t.Errorf("model = %q, want explicit deepseek-v4-pro", mc.Model)
	}
}

// opencode-go registry fallback resolves via SelectModel when not declared.
func TestSelectModelOpenCodeGoRegistryFallback(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "sk-opencode")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	mc, err := cfg.SelectModel("opencode-go")
	if err != nil {
		t.Fatalf("SelectModel(opencode-go) via registry: %v", err)
	}
	if mc.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Errorf("base_url = %q, want opencode-go endpoint", mc.BaseURL)
	}
	if mc.Model != "deepseek-v4-flash" {
		t.Errorf("model = %q, want registry default deepseek-v4-flash", mc.Model)
	}
	if mc.Provider != "openai" {
		t.Errorf("provider = %q, want openai", mc.Provider)
	}
}

// Per-model temperature from registry: Kimi K3 requires temperature=1.
func TestRegistryPerModelTemperature(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  opencode-go:
    model: kimi-k3
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["opencode-go"]
	if mc.Temperature != 1.0 {
		t.Errorf("temperature = %v, want 1.0 (Kimi K3 template default)", mc.Temperature)
	}
}

// Explicit temperature wins over the registry template default.
func TestRegistryPerModelTemperatureNotOverridden(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  opencode-go:
    model: kimi-k3
    temperature: 0.7
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["opencode-go"]
	if mc.Temperature != 0.7 {
		t.Errorf("temperature = %v, want explicit 0.7 preserved", mc.Temperature)
	}
}

// Models without a per-model temperature template still get the global default.
func TestRegistryGlobalTemperatureDefault(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  deepseek:
    model: deepseek-v4-pro
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["deepseek"]
	if mc.Temperature != 0.2 {
		t.Errorf("temperature = %v, want global default 0.2", mc.Temperature)
	}
}

// ollama: a local provider with no env key needed.
func TestRegistryOllamaLocalProvider(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
models:
  ollama:
    provider: ollama
`))
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Models["ollama"]
	if mc.Provider != "ollama" {
		t.Errorf("provider = %q, want ollama", mc.Provider)
	}
	if mc.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("base_url = %q, want ollama default", mc.BaseURL)
	}
	if !mc.Credential.IsZero() {
		t.Errorf("credential = %+v, want zero (local provider)", mc.Credential)
	}
}
