package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"code-agent/internal/app"
	"code-agent/internal/runtime"
	"code-agent/internal/server"
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

	// Model key injected — the model's Credential ref is aligned to llm/deepseek
	// (the deprecated APIKey field is no longer written, R1.1).
	if got := cfg.Models["deepseek"].Credential; got != (app.CredentialRef{Namespace: "llm", Name: "deepseek"}) {
		t.Errorf("model Credential ref = %+v, want llm/deepseek", got)
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
	const serverAccessToken = "0123456789abcdef0123456789abcdef"
	h, err := StartServer(context.Background(), Options{
		WorkspaceDir: workspace,
		DataDir:      t.TempDir(),
		ConfigYAML: `{
			"default_model": "",
			"models": {},
			"credentials": {},
			"web": {"fetch": {"timeout_seconds": 30}}
		}`,
		Sandboxed:         true,
		ServerAccessToken: serverAccessToken,
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
	baseURL := strings.Replace(h.LoopbackURL(), "ws://", "http://", 1)
	if response, err := http.Get(baseURL + "/healthz"); err != nil {
		t.Fatalf("public health request: %v", err)
	} else {
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("public health status = %d", response.StatusCode)
		}
	}
	if response, err := http.Get(baseURL + "/v1/runtime/info"); err != nil {
		t.Fatalf("unauthenticated info request: %v", err)
	} else {
		defer response.Body.Close()
		var envelope struct {
			Code int `json:"code"`
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		}
		if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode auth response: %v", err)
		}
		if response.StatusCode != http.StatusUnauthorized || envelope.Code != server.CodeUnauthorized ||
			envelope.Data.Code != "runtime_auth_required" {
			t.Fatalf("auth response status=%d envelope=%+v", response.StatusCode, envelope)
		}
	}
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/runtime/info", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+serverAccessToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("authenticated info request: %v", err)
	}
	defer response.Body.Close()
	var infoEnvelope struct {
		Code int                `json:"code"`
		Data server.RuntimeInfo `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&infoEnvelope); err != nil {
		t.Fatalf("decode Runtime info: %v", err)
	}
	if response.StatusCode != http.StatusOK || infoEnvelope.Code != 0 ||
		infoEnvelope.Data.ServerID == "" ||
		infoEnvelope.Data.RuntimeProfile != server.RuntimeProfileSandboxed {
		t.Fatalf("Runtime info status=%d envelope=%+v", response.StatusCode, infoEnvelope)
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
	if h.rt.Owner == nil {
		t.Fatal("Runtime Ownership manager was not assembled")
	}
	if err := h.rt.Owner.Heartbeat(context.Background()); err != nil {
		t.Fatalf("owner Heartbeat: %v", err)
	}
	if resolution, err := h.rt.Owner.Resolve(context.Background(), sess.ID); err != nil || !resolution.Local {
		t.Fatalf("owner Resolve = %+v, %v", resolution, err)
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
	if err := h.Reconfigure("", "{}", ""); err != nil {
		t.Fatalf("zero-model Reconfigure: %v", err)
	}
}

func TestStartServerRequiresHostGeneratedAccessToken(t *testing.T) {
	_, err := StartServer(context.Background(), Options{
		WorkspaceDir: t.TempDir(),
		DataDir:      t.TempDir(),
		ConfigYAML:   `{"default_model":"","models":{}}`,
		Sandboxed:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "in-memory Server Access Token") {
		t.Fatalf("StartServer error = %v, want embedded access-token requirement", err)
	}
}

func TestStartServerRejectsNonLoopbackPrivateListener(t *testing.T) {
	_, err := StartServer(context.Background(), Options{
		WorkspaceDir:      t.TempDir(),
		DataDir:           t.TempDir(),
		ConfigYAML:        `{"default_model":"","models":{}}`,
		Addr:              "0.0.0.0:0",
		Sandboxed:         true,
		ServerAccessToken: "0123456789abcdef0123456789abcdef",
	})
	if err == nil || !strings.Contains(err.Error(), "must bind to loopback") {
		t.Fatalf("StartServer error = %v, want loopback restriction", err)
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
