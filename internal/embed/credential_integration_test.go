package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"code-agent/internal/app"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/runtime"
)

// stubChatHandler returns a minimal valid chat completion response and records
// the Authorization header the client sent.
func stubChatHandler(t *testing.T, authHeader *string, status int, body string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body == "" && status == 200 {
			body = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		}
		w.Write([]byte(body))
	})
}

// TestE0_OldBYOK verifies that the legacy api_key_env config path still works
// and produces the correct Authorization header.
func TestE0_OldBYOK(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(stubChatHandler(t, &authHeader, 200, ""))
	defer srv.Close()

	// Old BYOK: no CredentialRef — falls through to static APIKey path.
	// This tests that models without credential: section still work.
	mc := app.ModelConfig{
		Provider:  "openai",
		BaseURL:   srv.URL,
		Model:     "test-model",
		APIKeyEnv: "DEEPSEEK_API_KEY",
		APIKey:    "sk-old-byok",
		// Credential is zero — the auto-derivation only fires when APIKey != "",
		// but the static path is used because Credential.IsZero() is true.
	}
	pc := app.ProviderConfig{
		RequestTimeoutSeconds: 10,
		MaxRetries:            0,
	}

	provider, err := runtime.BuildProvider(mc, pc, nil)
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}

	_, err = provider.Complete(context.Background(), model.Request{
		Model:    "test-model",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if authHeader != "Bearer sk-old-byok" {
		t.Errorf("Authorization = %q, want %q", authHeader, "Bearer sk-old-byok")
	}
}

// TestE0_NewGateway verifies the new credential injection path: secretsJSON
// with new format produces a StaticResolver, and the provider uses it.
func TestE0_NewGateway(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(stubChatHandler(t, &authHeader, 200, ""))
	defer srv.Close()

	cfgYAML := `
models:
  agent:
    provider: openai
    base_url: "` + srv.URL + `"
    model: "test-model"
    credential:
      namespace: gateway
      name: default
`
	cfg, err := app.LoadConfigBytes([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}

	// Simulate AgentKit injecting a gateway token via secretsJSON (new format).
	secrets := map[string]string{
		"gateway/default": `{"type":"bearer","secret":"jwt-gateway-token"}`,
	}
	injectedResolver := injectSecrets(&cfg, secrets)
	if injectedResolver == nil {
		t.Fatal("injectSecrets returned nil resolver for new-format secrets")
	}
	credChain := cfg.CredentialResolver(injectedResolver)

	mc, err := cfg.SelectModel("agent")
	if err != nil {
		t.Fatalf("SelectModel: %v", err)
	}

	pc := app.ProviderConfig{
		RequestTimeoutSeconds: 10,
		MaxRetries:            0,
	}
	provider, err := runtime.BuildProvider(mc, pc, credChain)
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}

	_, err = provider.Complete(context.Background(), model.Request{
		Model:    "test-model",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if authHeader != "Bearer jwt-gateway-token" {
		t.Errorf("Authorization = %q, want %q", authHeader, "Bearer jwt-gateway-token")
	}
}

// TestE0_ReconfigureHotUpdate verifies that after Reconfigure with a new token,
// subsequent requests use the new token.
func TestE0_ReconfigureHotUpdate(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(stubChatHandler(t, &authHeader, 200, ""))
	defer srv.Close()

	cfgYAML := `
models:
  agent:
    provider: openai
    base_url: "` + srv.URL + `"
    model: "test-model"
    credential:
      namespace: gateway
      name: default
`
	cfg, err := app.LoadConfigBytes([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}

	pc := app.ProviderConfig{
		RequestTimeoutSeconds: 10,
		MaxRetries:            0,
	}

	// --- Phase 1: inject token-v1 ---
	secretsV1 := map[string]string{
		"gateway/default": `{"type":"bearer","secret":"jwt-v1"}`,
	}
	cfgV1 := cfg // copy-on-stack, same as Reconfigure does
	injectedV1 := injectSecrets(&cfgV1, secretsV1)
	chainV1 := cfgV1.CredentialResolver(injectedV1)

	mcV1, err := cfgV1.SelectModel("agent")
	if err != nil {
		t.Fatalf("SelectModel v1: %v", err)
	}

	providerV1, err := runtime.BuildProvider(mcV1, pc, chainV1)
	if err != nil {
		t.Fatalf("BuildProvider v1: %v", err)
	}

	_, err = providerV1.Complete(context.Background(), model.Request{
		Model:    "test-model",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete v1: %v", err)
	}
	if authHeader != "Bearer jwt-v1" {
		t.Fatalf("v1 Authorization = %q, want %q", authHeader, "Bearer jwt-v1")
	}

	// --- Phase 2: reconfigure with token-v2 ---
	cfgV2 := cfg // fresh copy-on-stack
	secretsV2 := map[string]string{
		"gateway/default": `{"type":"bearer","secret":"jwt-v2"}`,
	}
	injectedV2 := injectSecrets(&cfgV2, secretsV2)
	chainV2 := cfgV2.CredentialResolver(injectedV2)

	mcV2, err := cfgV2.SelectModel("agent")
	if err != nil {
		t.Fatalf("SelectModel v2: %v", err)
	}

	providerV2, err := runtime.BuildProvider(mcV2, pc, chainV2)
	if err != nil {
		t.Fatalf("BuildProvider v2: %v", err)
	}

	_, err = providerV2.Complete(context.Background(), model.Request{
		Model:    "test-model",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete v2: %v", err)
	}
	if authHeader != "Bearer jwt-v2" {
		t.Errorf("v2 Authorization = %q, want %q after Reconfigure", authHeader, "Bearer jwt-v2")
	}
}

// TestE0_401NoRetry verifies that the Runtime does NOT retry on 401 —
// the error is propagated immediately so the host can refresh the token.
func TestE0_401NoRetry(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"Unauthorized","type":"auth_error","code":"unauthorized"}}`))
	}))
	defer srv.Close()

	cfgYAML := `
models:
  agent:
    provider: openai
    base_url: "` + srv.URL + `"
    model: "test-model"
    credential:
      namespace: gateway
      name: default
`
	cfg, err := app.LoadConfigBytes([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}

	secrets := map[string]string{
		"gateway/default": `{"type":"bearer","secret":"expired-jwt"}`,
	}
	injected := injectSecrets(&cfg, secrets)
	chain := cfg.CredentialResolver(injected)

	mc, err := cfg.SelectModel("agent")
	if err != nil {
		t.Fatalf("SelectModel: %v", err)
	}

	// MaxRetries=1: if 401 were retryable, we'd see 2 calls (1 initial + 1 retry).
	pc := app.ProviderConfig{
		RequestTimeoutSeconds: 10,
		MaxRetries:            1,
		BackoffMillis:         1,
		MaxBackoffSeconds:     1,
	}
	provider, err := runtime.BuildProvider(mc, pc, chain)
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}

	_, err = provider.Complete(context.Background(), model.Request{
		Model:    "test-model",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}

	// Key assertion: only ONE call was made — 401 is not retryable.
	if callCount != 1 {
		t.Errorf("callCount = %d, want 1 (401 must not be retried)", callCount)
	}

	// Verify the error contains an APIError with 401 (may be wrapped).
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *model.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}

// TestE0_GatewayWithExpiry verifies that expires_at from secretsJSON is
// preserved in the StaticResolver and flows through to the Credential.
func TestE0_GatewayWithExpiry(t *testing.T) {
	cfgYAML := `
models:
  agent:
    provider: openai
    base_url: "https://agent.example.com/api/v1/agent"
    model: "test-model"
    credential:
      namespace: gateway
      name: default
`
	cfg, err := app.LoadConfigBytes([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}

	secrets := map[string]string{
		"gateway/default": `{"type":"bearer","secret":"jwt-with-expiry","expires_at":2000000000}`,
	}
	injected := injectSecrets(&cfg, secrets)
	if injected == nil {
		t.Fatal("injectSecrets returned nil")
	}

	ctx := context.Background()
	target := credential.Target{Namespace: "gateway", Name: "default"}
	c, err := injected.Resolve(ctx, target)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Secret != "jwt-with-expiry" {
		t.Errorf("Secret = %q", c.Secret)
	}
	if c.ExpiresAt == nil {
		t.Fatal("ExpiresAt is nil, want non-nil from secretsJSON")
	}
	if c.ExpiresAt.Unix() != 2000000000 {
		t.Errorf("ExpiresAt.Unix() = %d, want 2000000000", c.ExpiresAt.Unix())
	}
}

// TestE0_InjectSecretsOldFormatStillWorks verifies that old-format secrets
// (env-var-name → plain string) still inject into APIKey.
func TestE0_InjectSecretsOldFormatStillWorks(t *testing.T) {
	cfg := app.Config{
		Models: map[string]app.ModelConfig{
			"deepseek": {APIKeyEnv: "DEEPSEEK_API_KEY"},
		},
	}

	secrets := map[string]string{
		"DEEPSEEK_API_KEY": "sk-legacy",
	}
	resolver := injectSecrets(&cfg, secrets)

	// Old format should not produce a resolver.
	if resolver != nil {
		t.Error("old-format secrets should return nil resolver")
	}

	// APIKey should still be set (backward compat).
	if cfg.Models["deepseek"].APIKey != "sk-legacy" {
		t.Errorf("APIKey = %q, want sk-legacy", cfg.Models["deepseek"].APIKey)
	}
}

// TestE0_ChainPriority verifies the resolver chain priority:
// StaticResolver (injected) wins over EnvResolver (fallback).
func TestE0_ChainPriority(t *testing.T) {
	ctx := context.Background()
	target := credential.Target{Namespace: "gateway", Name: "default"}

	static := credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "injected-wins"},
	}
	env := &credential.EnvResolver{}

	chain := &credential.ChainResolver{
		Resolvers: []credential.Resolver{static, env},
	}

	c, err := chain.Resolve(ctx, target)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Secret != "injected-wins" {
		t.Errorf("Secret = %q, want %q (injected should win)", c.Secret, "injected-wins")
	}
	if c.Source != "chain[0]=static:gateway/default" {
		t.Errorf("Source = %q, want %q", c.Source, "chain[0]=static:gateway/default")
	}
}

// TestE0_ReconfigurePreservesOldCredentialOnError verifies the atomicity
// guarantee: if a Reconfigure secretsJSON is malformed, the old credential
// state is preserved.
func TestE0_ReconfigurePreservesOldCredentialOnError(t *testing.T) {
	cfgYAML := `
models:
  agent:
    provider: openai
    base_url: "https://agent.example.com/api/v1/agent"
    model: "test-model"
    credential:
      namespace: gateway
      name: default
`
	cfg, err := app.LoadConfigBytes([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}

	// Inject valid token first.
	validSecrets := map[string]string{
		"gateway/default": `{"type":"bearer","secret":"valid-token"}`,
	}
	injected := injectSecrets(&cfg, validSecrets)
	if injected == nil {
		t.Fatal("valid injection returned nil")
	}

	// Verify it resolves.
	ctx := context.Background()
	target := credential.Target{Namespace: "gateway", Name: "default"}
	c, _ := injected.Resolve(ctx, target)
	if c.Secret != "valid-token" {
		t.Fatalf("expected valid-token, got %q", c.Secret)
	}

	// Now try to inject malformed JSON — should be skipped, old value preserved.
	cfgCopy := cfg
	malformedSecrets := map[string]string{
		"gateway/default": `{"type":"bearer","secret":`,
	}
	injectedMalformed := injectSecrets(&cfgCopy, malformedSecrets)
	// Malformed value is skipped by parseCredentialValue; resolver may be nil
	// but the old APIKey on the model should still be "valid-token".
	_ = injectedMalformed

	// Re-verify old token: the model in the ORIGINAL cfg still points to valid-token.
	if cfg.Models["agent"].APIKey != "valid-token" {
		t.Errorf("APIKey = %q, want valid-token (old state preserved)", cfg.Models["agent"].APIKey)
	}

	// The original resolver still returns the valid token.
	c2, err := injected.Resolve(ctx, target)
	if err != nil {
		t.Fatalf("Resolve after malformed attempt: %v", err)
	}
	if c2.Secret != "valid-token" {
		t.Errorf("Secret = %q, want valid-token (resolver preserved)", c2.Secret)
	}
}

// TestE0_InjectSecretsNewFormatPreservesExpiry verifies that the expires_at
// from secretsJSON survives the full injection → resolver → chain → provider cycle.
func TestE0_InjectSecretsNewFormatPreservesExpiry(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(stubChatHandler(t, &authHeader, 200, ""))
	defer srv.Close()

	cfgYAML := `
models:
  agent:
    provider: openai
    base_url: "` + srv.URL + `"
    model: "test-model"
    credential:
      namespace: gateway
      name: default
`
	cfg, err := app.LoadConfigBytes([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}

	futureExpiry := time.Now().Add(24 * time.Hour).Unix()
	secretsJSON := fmt.Sprintf(`{"gateway/default":{"type":"bearer","secret":"jwt-expiring","expires_at":%d}}`, futureExpiry)
	secrets, err := parseSecretsJSON(secretsJSON)
	if err != nil {
		t.Fatalf("parseSecretsJSON: %v", err)
	}

	injected := injectSecrets(&cfg, secrets)
	chain := cfg.CredentialResolver(injected)

	ctx := context.Background()
	target := credential.Target{Namespace: "gateway", Name: "default"}
	c, err := chain.Resolve(ctx, target)
	if err != nil {
		t.Fatalf("chain.Resolve: %v", err)
	}
	if c.IsExpired() {
		t.Error("token with future expiry should not be expired")
	}
	if c.Secret != "jwt-expiring" {
		t.Errorf("Secret = %q", c.Secret)
	}
	if c.ExpiresAt == nil || c.ExpiresAt.Unix() != futureExpiry {
		t.Errorf("ExpiresAt = %v, want unix %d", c.ExpiresAt, futureExpiry)
	}
}

