package embed

import (
	"testing"

	"code-agent/internal/app"
)

func TestInjectSecrets_WebSearchKeys(t *testing.T) {
	cfg := app.Config{
		Models: map[string]app.ModelConfig{
			"deepseek": {APIKeyEnv: "DEEPSEEK_API_KEY"},
		},
	}
	cfg.Web.Search.TavilyAPIKeyEnv = "TAVILY_API_KEY"
	cfg.Web.Search.BraveAPIKeyEnv = "BRAVE_API_KEY"

	secrets := map[string]string{
		"DEEPSEEK_API_KEY": "sk-model",
		"TAVILY_API_KEY":   "tvly-keychain",
		"BRAVE_API_KEY":    "brave-keychain",
	}

	injectSecrets(&cfg, secrets)

	// Model key injected.
	if cfg.Models["deepseek"].APIKey != "sk-model" {
		t.Errorf("model APIKey = %q, want sk-model", cfg.Models["deepseek"].APIKey)
	}

	// Web search keys injected.
	if cfg.Web.Search.TavilyKey != "tvly-keychain" {
		t.Errorf("TavilyKey = %q, want tvly-keychain", cfg.Web.Search.TavilyKey)
	}
	if cfg.Web.Search.BraveKey != "brave-keychain" {
		t.Errorf("BraveKey = %q, want brave-keychain", cfg.Web.Search.BraveKey)
	}

	// Getters prefer the injected key.
	if got := cfg.Web.Search.TavilyAPIKey(); got != "tvly-keychain" {
		t.Errorf("TavilyAPIKey() = %q, want tvly-keychain", got)
	}
	if got := cfg.Web.Search.BraveAPIKey(); got != "brave-keychain" {
		t.Errorf("BraveAPIKey() = %q, want brave-keychain", got)
	}
}

func TestInjectSecrets_WebSearchNoEnvName(t *testing.T) {
	// When tavily_api_key_env is empty, no injection should happen even if a
	// matching secret key exists — there's no declared env name to match against.
	cfg := app.Config{
		Models: map[string]app.ModelConfig{
			"deepseek": {APIKeyEnv: "DEEPSEEK_API_KEY"},
		},
	}
	// Web search is at defaults: TavilyAPIKeyEnv is empty.

	secrets := map[string]string{
		"DEEPSEEK_API_KEY": "sk-model",
		"TAVILY_API_KEY":   "tvly-keychain",
	}

	injectSecrets(&cfg, secrets)

	if cfg.Web.Search.TavilyKey != "" {
		t.Errorf("TavilyKey = %q, want empty (no api_key_env to match)", cfg.Web.Search.TavilyKey)
	}
}

func TestInjectSecrets_WebSearchEmptySecret(t *testing.T) {
	cfg := app.Config{
		Models: map[string]app.ModelConfig{
			"deepseek": {APIKeyEnv: "DEEPSEEK_API_KEY"},
		},
	}
	cfg.Web.Search.TavilyAPIKeyEnv = "TAVILY_API_KEY"

	// Secret key present but value is empty → skipped.
	secrets := map[string]string{
		"DEEPSEEK_API_KEY": "sk-model",
		"TAVILY_API_KEY":   "",
	}

	injectSecrets(&cfg, secrets)

	if cfg.Web.Search.TavilyKey != "" {
		t.Errorf("TavilyKey = %q, want empty (secret value was empty)", cfg.Web.Search.TavilyKey)
	}
}

func TestInjectSecrets_NilSecrets(t *testing.T) {
	cfg := app.Config{
		Models: map[string]app.ModelConfig{
			"deepseek": {APIKeyEnv: "DEEPSEEK_API_KEY", APIKey: "already-set"},
		},
	}
	cfg.Web.Search.TavilyAPIKeyEnv = "TAVILY_API_KEY"

	// Empty map → no-op, must not clear existing values.
	injectSecrets(&cfg, map[string]string{})

	if cfg.Models["deepseek"].APIKey != "already-set" {
		t.Error("empty secrets cleared existing model key")
	}
	if cfg.Web.Search.TavilyKey != "" {
		t.Error("empty secrets set a web key")
	}
}
