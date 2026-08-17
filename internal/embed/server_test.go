package embed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/app"
	"code-agent/internal/credential"
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

// TestEmbeddedProvidersPersistToDataDir verifies the embedded runtime exposes
// the daemon-parity /v1/providers surface: PUT persists to
// <DataDir>/.codeagent/settings.json (applied=false, restart required), GET
// round-trips the definition, and a second StartServer over the same DataDir
// loads the persisted provider back into the config.
func TestEmbeddedProvidersPersistToDataDir(t *testing.T) {
	const serverAccessToken = "0123456789abcdef0123456789abcdef"
	dataDir := t.TempDir()
	workspace := t.TempDir()

	start := func() *Handle {
		h, err := StartServer(context.Background(), Options{
			WorkspaceDir: workspace,
			DataDir:      dataDir,
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
			t.Fatalf("StartServer: %v", err)
		}
		return h
	}
	h := start()
	defer h.Stop()
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		baseURL := strings.Replace(h.LoopbackURL(), "ws://", "http://", 1)
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, baseURL+path, reader)
		if err != nil {
			t.Fatalf("new request %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+serverAccessToken)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var envelope map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
		return resp.StatusCode, envelope
	}

	status, envelope := do(http.MethodPut, "/v1/providers/dashscope", `{
		"enabled": true,
		"base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
		"api": "openai",
		"credential": {"namespace": "llm", "name": "dashscope"},
		"models": [{"id": "qwen3-coder-plus", "runtime_alias": "qwen3 coder plus"}]
	}`)
	if status != http.StatusOK {
		t.Fatalf("PUT /v1/providers status = %d envelope=%+v", status, envelope)
	}
	data, _ := envelope["data"].(map[string]any)
	if applied, _ := data["applied"].(bool); applied {
		t.Fatalf("PUT applied = true, want false (restart required): %+v", envelope)
	}

	// GET /v1/providers — definition round-trips.
	status, envelope = do(http.MethodGet, "/v1/providers", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/providers status = %d envelope=%+v", status, envelope)
	}
	providers, _ := envelope["data"].(map[string]any)["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("GET /v1/providers providers = %d, want 1: %+v", len(providers), envelope)
	}

	// The definition landed on disk under DataDir.
	settingsPath := filepath.Join(dataDir, ".codeagent", "settings.json")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read persisted settings.json: %v", err)
	}
	if !strings.Contains(string(raw), "dashscope") {
		t.Fatalf("persisted settings.json missing dashscope: %s", raw)
	}

	// Restart over the same DataDir: the persisted provider is loaded back.
	h.Stop()
	h = start()
	defer h.Stop()
	status, envelope = do(http.MethodGet, "/v1/providers", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/providers after restart status = %d envelope=%+v", status, envelope)
	}
	providers, _ = envelope["data"].(map[string]any)["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("GET /v1/providers after restart = %d, want 1: %+v", len(providers), envelope)
	}
}

// TestEmbeddedSecretsInjection verifies POST /v1/secrets updates the mutable
// resolver and the next GET /v1/runtime/models reflects the injected credential
// (A2 parity with codeagentd).
func TestEmbeddedSecretsInjection(t *testing.T) {
	const serverAccessToken = "0123456789abcdef0123456789abcdef"
	h, err := StartServer(context.Background(), Options{
		WorkspaceDir: t.TempDir(),
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
		t.Fatalf("StartServer: %v", err)
	}
	defer h.Stop()

	baseURL := strings.Replace(h.LoopbackURL(), "ws://", "http://", 1)
	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+serverAccessToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		var envelope map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode POST %s: %v", path, err)
		}
		return resp.StatusCode, envelope
	}

	status, envelope := post("/v1/secrets", `{"llm/dashscope": {"type": "bearer", "secret": "sk-test"}}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/secrets status = %d envelope=%+v", status, envelope)
	}
	if injected, _ := envelope["data"].(map[string]any)["injected"].(float64); injected != 1 {
		t.Fatalf("POST /v1/secrets injected = %v, want 1: %+v", injected, envelope)
	}

	// The mutable resolver now serves the injected credential.
	if got, err := h.credential.Resolve(context.Background(), credential.Target{Namespace: "llm", Name: "dashscope"}); err != nil || got.Secret != "sk-test" {
		t.Fatalf("resolved injected credential = %+v, %v; want secret sk-test", got, err)
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
			"deepseek": {APIKeyEnv: "DEEPSEEK_API_KEY"},
		},
	}
	cfg.Web.Search.TavilyAPIKeyEnv = "TAVILY_API_KEY"

	// Empty map → no-op, must not clear existing values.
	injectSecrets(&cfg, map[string]string{})

	if cfg.Models["deepseek"].APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Error("empty secrets cleared existing APIKeyEnv")
	}
	if cfg.Web.Search.TavilyKey != "" {
		t.Error("empty secrets set a web key")
	}
}
