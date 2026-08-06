package app

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// aliasKey builds the flat model map key for a grouped provider model
// (design-providers-grouped-config.md §3.3): provider.<b64url(pid)>.model.<b64url(mid)>.
// base64url keeps the key slash-free even when the model id contains "/"
// (e.g. OpenRouter's deepseek/deepseek-chat), so it can never collide with a
// user-authored flat models key.
func aliasKey(pid, mid string) string {
	return "provider." + b64urlEncode(pid) + ".model." + b64urlEncode(mid)
}

// b64urlEncode base64url-encodes s without padding (matches the runtime alias
// encoding in /v1/runtime/models).
func b64urlEncode(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// b64urlDecode base64url-decodes s; returns ok=false on invalid input.
func b64urlDecode(s string) (string, bool) {
	buf, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	return string(buf), true
}

// DisplayModelName returns a human-readable label for a model key. A grouped-
// provider model (alias key provider.<b64>.model.<b64>) becomes
// "<provider-id>/<model-id>"; any other key is returned unchanged. The TUI
// /use picker and REPL /models use this so users never see base64 alias keys.
func (c Config) DisplayModelName(key string) string {
	mc, ok := c.Models[key]
	if !ok {
		return key
	}
	if mc.Catalog.ConnectionID == "" {
		return key // flat model, key is already the friendly name
	}
	display := mc.Catalog.DisplayName
	if display == "" {
		display = mc.Model
	}
	return mc.Catalog.ConnectionID + "/" + display
}

// validateFlatModelKey enforces design-providers-grouped-config.md §3.1: a
// user-authored flat models key must not contain "/". Grouped-provider models
// expand to alias keys (provider.<b64>.model.<b64>) which are slash-free, so a
// "/" in a flat key means the user is trying to write a provider-scoped model
// in the flat form — which would collide with the grouped expansion. Returns an
// error naming the offending key.
func validateFlatModelKey(name string) error {
	if strings.Contains(name, "/") {
		return fmt.Errorf("models key %q must not contain \"/\"; use the providers section for provider-scoped models", name)
	}
	return nil
}

// Built-in connection registry (design-connection-flattening §8.3 层级 1).
//
// Known connections get their base URL and conventional env-var name for the
// API key from here, so a user does not have to remember endpoint URLs. The
// registry fills fields ONLY when a model config leaves them empty — an
// explicit base_url or api_key_env in YAML always wins. Context window and
// pricing are deliberately NOT in the registry yet: the generated model
// catalog (a post-PRD work item) owns capability data; until then the generic
// defaultContextWindow / unpriced defaults apply.

type builtinConnection struct {
	// BaseURL is the conventional endpoint. Empty means "injected or
	// configured" (the gateway connection has no single built-in endpoint).
	BaseURL string
	// Env is the conventional API-key env var, e.g. "DEEPSEEK_API_KEY".
	// Empty means the connection needs no env key (ollama local, gateway
	// injected).
	Env string
	// WireModel is the OpenAI-compatible model string sent in the request body
	// for this connection. When empty (gateway / ollama) the caller must supply
	// one — SelectModel's registry fallback will expose the connection name
	// but the actual request will fail until a user-declared model entry sets
	// a concrete wire model.
	WireModel string
	// ProviderType is the wire protocol this service speaks ("openai" =
	// chat-completions compatible, "responses", "ollama"). design-provider-id-
	// model.md: a known service id resolves to this api type; unknown services
	// fall back to the generic openai/responses paths.
	ProviderType string
}

var builtinConnections = map[string]builtinConnection{
	"deepseek":   {BaseURL: "https://api.deepseek.com", Env: "DEEPSEEK_API_KEY", WireModel: "deepseek-v4-flash", ProviderType: "openai"},
	"qwen":       {BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Env: "DASHSCOPE_API_KEY", WireModel: "qwen3-coder-plus", ProviderType: "openai"},
	"glm":        {BaseURL: "https://open.bigmodel.cn/api/paas/v4", Env: "GLM_API_KEY", WireModel: "glm-4.7", ProviderType: "openai"},
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", Env: "OPENROUTER_API_KEY", ProviderType: "openai"}, // model ids may contain "/" (e.g. deepseek/deepseek-chat)
	"ollama":     {BaseURL: "http://localhost:11434/v1", ProviderType: "ollama"},                               // user declares a modelfile name in config
	"gateway":    {ProviderType: "openai"},                                                                     // base_url/env/wire_model supplied by the host via injection
}

// applyRegistryDefaults fills unset BaseURL/APIKeyEnv from the built-in
// connection registry, keyed by the model's friendly name. Explicit values in
// the config are never overwritten.
//
// design-provider-id-model.md: when the user writes a known service id (e.g.
// "provider: deepseek" with a model also named deepseek), the registry also
// resolves the service id to its api type (ProviderType) and records the
// service id in Catalog.ProviderID for /v1/runtime/models. A generic api type
// (openai/responses/ollama) or an explicit base_url is never overwritten.
func applyRegistryDefaults(mc *ModelConfig) {
	conn, ok := builtinConnections[mc.Name]
	if !ok {
		return
	}
	if mc.BaseURL == "" {
		mc.BaseURL = conn.BaseURL
	}
	if mc.APIKeyEnv == "" {
		mc.APIKeyEnv = conn.Env
	}
	if conn.ProviderType != "" && (mc.Provider == "" || mc.Provider == mc.Name) {
		mc.Provider = conn.ProviderType
	}
	if mc.Catalog.ProviderID == "" {
		mc.Catalog.ProviderID = mc.Name
	}
}

// AvailableModelNames returns the model names the TUI/REPL should offer: the
// config-declared models plus the built-in registry connections, deduplicated
// and sorted. This is what makes the picker "open-box" — a fresh directory
// with no config.yaml still offers qwen/glm/ollama alongside the default
// deepseek (R2/T1.3). Selection itself goes through SelectModel, which errors
// clearly when the chosen connection has no credential.
func (c Config) AvailableModelNames() []string {
	seen := make(map[string]struct{}, len(c.Models)+len(builtinConnections))
	for name := range c.Models {
		seen[name] = struct{}{}
	}
	for name := range builtinConnections {
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
