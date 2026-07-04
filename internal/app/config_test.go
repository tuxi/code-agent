package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigMultiModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yaml := `
default_model: qwen
models:
  deepseek:
    provider: openai
    base_url: https://api.deepseek.com
    model: deepseek-v4-pro
    api_key_env: DEEPSEEK_API_KEY
  qwen:
    provider: openai
    base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
    model: qwen3-coder-plus
    api_key_env: DASHSCOPE_API_KEY
agent:
  max_steps: 12
workspace:
  root: .
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Make key presence deterministic regardless of the caller's environment.
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("DASHSCOPE_API_KEY", "test-key")

	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModel != "qwen" {
		t.Errorf("default_model = %q, want qwen", cfg.DefaultModel)
	}
	if cfg.Agent.MaxSteps != 12 {
		t.Errorf("max_steps = %d, want 12", cfg.Agent.MaxSteps)
	}

	// Default selection (qwen) has its key set -> succeeds.
	mc, err := cfg.SelectModel("")
	if err != nil {
		t.Fatalf("select default: %v", err)
	}
	if mc.Name != "qwen" || mc.Model != "qwen3-coder-plus" {
		t.Errorf("selected %q/%q, want qwen/qwen3-coder-plus", mc.Name, mc.Model)
	}
	if mc.Temperature != 0.2 {
		t.Errorf("temperature = %v, want default 0.2", mc.Temperature)
	}
	// context_window is unset in this config -> default; compact_ratio defaults to
	// 0.7, so the threshold is model-aware off the default window.
	if mc.ContextWindow != 128000 {
		t.Errorf("context_window = %d, want default 128000", mc.ContextWindow)
	}
	if cfg.Agent.CompactRatio != 0.5 {
		t.Errorf("compact_ratio = %v, want default 0.5", cfg.Agent.CompactRatio)
	}
	if got := cfg.CompactThreshold(mc); got != 64000 {
		t.Errorf("CompactThreshold = %d, want 64000 (128000 * 0.5)", got)
	}
	// No provider section in this config -> transport defaults apply.
	if cfg.Provider.RequestTimeoutSeconds != 120 || cfg.Provider.MaxRetries != 2 ||
		cfg.Provider.BackoffMillis != 500 || cfg.Provider.MaxBackoffSeconds != 8 {
		t.Errorf("provider defaults not applied: %+v", cfg.Provider)
	}

	// deepseek is configured but its key is unset -> selection fails clearly.
	if _, err := cfg.SelectModel("deepseek"); err == nil {
		t.Error("expected an error selecting deepseek with no API key")
	}

	// Unknown model -> error.
	if _, err := cfg.SelectModel("gpt"); err == nil {
		t.Error("expected an error selecting an unknown model")
	}
}

func TestCompactThresholdIsModelAware(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yaml := `
default_model: big
models:
  big:
    provider: openai
    base_url: https://example.com
    model: big-model
    api_key_env: BIG_KEY
    context_window: 256000
  small:
    provider: openai
    base_url: https://example.com
    model: small-model
    api_key_env: SMALL_KEY
agent:
  compact_ratio: 0.8
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BIG_KEY", "k")
	t.Setenv("SMALL_KEY", "k")

	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}

	big, _ := cfg.SelectModel("big")
	small, _ := cfg.SelectModel("small")

	// Explicit window honored; unset window falls back to the default.
	if big.ContextWindow != 256000 {
		t.Errorf("big context_window = %d, want 256000", big.ContextWindow)
	}
	if small.ContextWindow != 128000 {
		t.Errorf("small context_window = %d, want default 128000", small.ContextWindow)
	}
	// Same ratio (0.8), different windows -> different thresholds. This is the
	// model-aware property P3.2 adds.
	if got := cfg.CompactThreshold(big); got != 204800 {
		t.Errorf("big threshold = %d, want 204800 (256000 * 0.8)", got)
	}
	if got := cfg.CompactThreshold(small); got != 102400 {
		t.Errorf("small threshold = %d, want 102400 (128000 * 0.8)", got)
	}
}

func TestWebSearchKeyResolution(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yaml := `
default_model: deepseek
models:
  deepseek:
    provider: openai
    base_url: https://api.deepseek.com
    model: deepseek-v4-pro
    api_key_env: DEEPSEEK_API_KEY
web:
  search:
    provider: tavily
    tavily_api_key_env: TAVILY_API_KEY
    brave_api_key_env: BRAVE_API_KEY
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Env key is set → resolved during normalization.
	t.Setenv("TAVILY_API_KEY", "tvly-env-key")
	t.Setenv("BRAVE_API_KEY", "brave-env-key")

	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.Web.Search.TavilyAPIKey(); got != "tvly-env-key" {
		t.Errorf("TavilyAPIKey = %q, want tvly-env-key (from env)", got)
	}
	if got := cfg.Web.Search.BraveAPIKey(); got != "brave-env-key" {
		t.Errorf("BraveAPIKey = %q, want brave-env-key (from env)", got)
	}

	// Direct key takes precedence over env.
	cfg.Web.Search.TavilyKey = "tvly-keychain"
	if got := cfg.Web.Search.TavilyAPIKey(); got != "tvly-keychain" {
		t.Errorf("TavilyAPIKey = %q, want tvly-keychain (direct key overrides env)", got)
	}

	// Unset env var and no direct key → empty.
	cfg.Web.Search.TavilyKey = ""
	t.Setenv("TAVILY_API_KEY", "")
	if got := cfg.Web.Search.TavilyAPIKey(); got != "" {
		t.Errorf("TavilyAPIKey = %q, want empty (no key set)", got)
	}
}

func TestWebSearchKeyDefaults(t *testing.T) {
	// Empty config → web search gets defaults but no keys (no env, no injection).
	cfg, err := LoadConfigBytes(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.Search.Provider != "tavily" {
		t.Errorf("provider = %q, want tavily (default)", cfg.Web.Search.Provider)
	}
	if cfg.Web.Search.TopK != 5 {
		t.Errorf("top_k = %d, want 5", cfg.Web.Search.TopK)
	}
	if cfg.Web.Search.TavilyAPIKey() != "" {
		t.Errorf("TavilyAPIKey = %q, want empty (no env, no injection)", cfg.Web.Search.TavilyAPIKey())
	}
}

func TestLoadConfigFallsBackToDeepseek(t *testing.T) {
	// No file, no models configured -> built-in deepseek default.
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModel != "deepseek" {
		t.Errorf("default_model = %q, want deepseek", cfg.DefaultModel)
	}
	if _, ok := cfg.Models["deepseek"]; !ok {
		t.Error("expected a built-in deepseek model")
	}
}
