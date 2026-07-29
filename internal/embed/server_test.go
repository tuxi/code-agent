package embed

import (
	"context"
	"errors"
	"testing"

	"code-agent/internal/app"
	"code-agent/internal/runtime"
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

func TestStartServerWithZeroModelsSupportsReadOnlyWorkspace(t *testing.T) {
	workspace := t.TempDir()
	h, err := StartServer(context.Background(), Options{
		WorkspaceDir: workspace,
		DataDir:      t.TempDir(),
		ConfigYAML: `{
			"default_model": "",
			"models": {},
			"credentials": {},
			"web": {"fetch": {"timeout_seconds": 30}}
		}`,
		Sandboxed: true,
	})
	if err != nil {
		t.Fatalf("StartServer zero models: %v", err)
	}
	defer h.Stop()

	if h.rt == nil || h.rt.Builder == nil {
		t.Fatal("zero-model Runtime did not assemble")
	}
	if _, ok := h.rt.Builder.ToolReg.Get("web_search"); ok {
		t.Fatal("zero-model Runtime registered unconfigured web_search")
	}
	if _, ok := h.rt.Builder.ToolReg.Get("web_fetch"); !ok {
		t.Fatal("zero-model Runtime must retain web_fetch")
	}

	sess, err := h.rt.Repo.Create(context.Background(), workspace, "")
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if _, err := h.rt.Repo.Load(context.Background(), sess.ID); err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if listed, err := h.rt.Repo.List(context.Background()); err != nil || len(listed) != 1 {
		t.Fatalf("list conversations = %d, %v", len(listed), err)
	}

	_, err = h.rt.Executor.ExecuteWithRequestID(
		context.Background(), sess.ID, "request-zero-model", "hello", "",
	)
	var notConfigured runtime.ModelNotConfiguredError
	if !errors.As(err, &notConfigured) {
		t.Fatalf("send error = %T %v, want ModelNotConfiguredError", err, err)
	}
	if notConfigured.AgentInputErrorCode() != "model_not_configured" {
		t.Fatalf("error code = %q", notConfigured.AgentInputErrorCode())
	}
	if err := h.Reconfigure("{}", ""); err != nil {
		t.Fatalf("zero-model Reconfigure: %v", err)
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
