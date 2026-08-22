package app

import (
	"code-agent/internal/settings"
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

type builtinModelTemplate struct {
	ID                string   // wire model id
	RuntimeAlias      string   // short friendly name (optional)
	ContextWindow     int      // token limit
	SupportsTools     bool     // tool calling
	SupportsReasoning bool     // reasoning/thinking
	InputModalities   []string // "text", "image", "audio"
	WebSearch         bool     // supports web search tool
	InputPricePerM    float64  // USD per million input tokens
	OutputPricePerM   float64  // USD per million output tokens
	Temperature       float64  // provider-recommended default (0 = use global default)
}

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
	// DisplayName is the human-readable service name for UI templates.
	DisplayName string
	// Summary is a one-line description for the template card.
	Summary string
	// Kind classifies the template: "api_key", "local", or "gateway".
	Kind string
	// Models are the suggested models for this provider template.
	Models []builtinModelTemplate
}

var builtinConnections = map[string]builtinConnection{
	"deepseek": {
		BaseURL: "https://api.deepseek.com", Env: "DEEPSEEK_API_KEY",
		WireModel: "deepseek-v4-flash", ProviderType: "openai",
		DisplayName: "DeepSeek", Summary: "使用 DeepSeek API 密钥连接", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "deepseek-v4-flash", RuntimeAlias: "deepseek", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, WebSearch: true, InputPricePerM: 0.16, OutputPricePerM: 0.32},
			{ID: "deepseek-v4-pro", RuntimeAlias: "deepseek-pro", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.45, OutputPricePerM: 0.90},
			{ID: "deepseek-v4-flash-vision-exp", RuntimeAlias: "deepseek-vision", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text", "image"}, InputPricePerM: 0.16, OutputPricePerM: 0.32},
		},
	},
	"qwen": {
		BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Env: "DASHSCOPE_API_KEY",
		WireModel: "qwen3-coder-plus", ProviderType: "openai",
		DisplayName: "Alibaba Qwen", Summary: "使用阿里云百炼 OpenAI 兼容接口", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "qwen3-coder-plus", ContextWindow: 128_000, SupportsTools: true, InputModalities: []string{"text"}},
		},
	},
	"glm": {
		BaseURL: "https://open.bigmodel.cn/api/paas/v4", Env: "GLM_API_KEY",
		WireModel: "glm-4.7", ProviderType: "openai",
		DisplayName: "Zhipu GLM", Summary: "使用智谱 OpenAI 兼容接口", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "glm-4.7", ContextWindow: 128_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}},
		},
	},
	"openrouter": {
		BaseURL: "https://openrouter.ai/api/v1", Env: "OPENROUTER_API_KEY",
		ProviderType: "openai",
		DisplayName:  "OpenRouter", Summary: "通过一个 API 密钥使用多个模型", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "openrouter/auto", RuntimeAlias: "openrouter", ContextWindow: 200_000, SupportsTools: true, InputModalities: []string{"text"}},
		},
	},
	"ollama": {
		BaseURL: "http://localhost:11434/v1", ProviderType: "ollama",
		DisplayName: "Ollama", Summary: "连接本机或局域网中的 Ollama", Kind: "local",
	},
	//"gateway": {
	//	ProviderType: "openai",
	//	DisplayName:  "Talkify Gateway", Summary: "使用 Talkify 账户、订阅模型和云端能力", Kind: "gateway",
	//},
	"opencode-go": {
		BaseURL: "https://opencode.ai/zen/go/v1", Env: "OPENCODE_GO_API_KEY",
		WireModel: "deepseek-v4-flash", ProviderType: "openai",
		DisplayName: "OpenCode Go", Summary: "低订阅费开源编程模型（首月 $5，之后 $10/月）", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "gpt-5.6-luna", RuntimeAlias: "opencode gpt 5.6 luna", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.22, OutputPricePerM: 0.66},
			{ID: "deepseek-v4-flash", RuntimeAlias: "opencode deepseek flash", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.22, OutputPricePerM: 0.66},
			{ID: "deepseek-v4-pro", RuntimeAlias: "opencode deepseek pro", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.66, OutputPricePerM: 1.98},
			{ID: "kimi-k3", RuntimeAlias: "opencode kimi k3", ContextWindow: 1_000_000, SupportsTools: true, InputModalities: []string{"text"}, InputPricePerM: 3.00, OutputPricePerM: 15.00, Temperature: 1.0},
			{ID: "kimi-k2.7-code", RuntimeAlias: "opencode kimi k2.7 code", ContextWindow: 256_000, SupportsTools: true, InputModalities: []string{"text"}, InputPricePerM: 1.2, OutputPricePerM: 5.6, Temperature: 1.0},
			{ID: "glm-5.2", RuntimeAlias: "opencode glm 5.2", ContextWindow: 128_000, SupportsTools: true, InputModalities: []string{"text"}, InputPricePerM: 1.40, OutputPricePerM: 4.40, Temperature: 0.2},
			{ID: "mimo-v2.5", RuntimeAlias: "opencode mimo v2.5", ContextWindow: 1_000_000, SupportsTools: true, InputModalities: []string{"text"}, InputPricePerM: 0.22, OutputPricePerM: 0.66, Temperature: 0.2},
		},
	},
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
func applyRegistryDefaults(mc *settings.ModelConfig) {
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
	// Per-model temperature: some providers (e.g. OpenCode Go / Kimi K3)
	// reject the global default of 0.2 and require a specific value.
	if mc.Temperature <= 0 && mc.Model != "" && len(conn.Models) > 0 {
		for _, m := range conn.Models {
			if m.ID == mc.Model && m.Temperature > 0 {
				mc.Temperature = m.Temperature
				break
			}
		}
	}
}

// AvailableModelNames returns the model names the TUI/REPL should offer: the
// config-declared models plus the built-in registry connections, deduplicated
// and sorted. This is what makes the picker "open-box" — a fresh directory
// with no config.yaml still offers qwen/glm/ollama alongside the default
// deepseek (R2/T1.3). Selection itself goes through SelectModel, which errors
// clearly when the chosen connection has no credential.
func AvailableModelNames(c settings.Settings) []string {
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

// BuiltinProviderTemplate is the exported shape of a built-in provider template,
// consumed by the /v1/provider-templates endpoint.
type BuiltinProviderTemplate struct {
	ID          string
	DisplayName string
	Summary     string
	Kind        string
	BaseURL     string
	API         string
	Env         string
	Models      []BuiltinProviderTemplateModel
}

// BuiltinProviderTemplateModel is one suggested model in a provider template.
type BuiltinProviderTemplateModel struct {
	ID                string
	RuntimeAlias      string
	ContextWindow     int
	SupportsTools     bool
	SupportsReasoning bool
	InputModalities   []string
	WebSearch         bool
	InputPricePerM    float64
	OutputPricePerM   float64
	Temperature       float64
}

// BuiltinConnection returns the built-in template for a known connection id.
// The second return is false for unknown ids (custom providers configured
// directly, not derived from the registry).
func BuiltinConnection(id string) (BuiltinProviderTemplate, bool) {
	conn, ok := builtinConnections[id]
	if !ok {
		return BuiltinProviderTemplate{}, false
	}
	return BuiltinProviderTemplate{
		ID:          id,
		DisplayName: conn.DisplayName,
		Summary:     conn.Summary,
		Kind:        conn.Kind,
		BaseURL:     conn.BaseURL,
		API:         conn.ProviderType,
		Env:         conn.Env,
	}, true
}

// BuiltinProviderTemplates returns the built-in provider templates derived from
// the connection registry.
func BuiltinProviderTemplates() []BuiltinProviderTemplate {
	out := make([]BuiltinProviderTemplate, 0, len(builtinConnections))
	for id, conn := range builtinConnections {
		t := BuiltinProviderTemplate{
			ID:          id,
			DisplayName: conn.DisplayName,
			Summary:     conn.Summary,
			Kind:        conn.Kind,
			BaseURL:     conn.BaseURL,
			API:         conn.ProviderType,
			Env:         conn.Env,
		}
		for _, m := range conn.Models {
			t.Models = append(t.Models, BuiltinProviderTemplateModel{
				ID:                m.ID,
				RuntimeAlias:      m.RuntimeAlias,
				ContextWindow:     m.ContextWindow,
				SupportsTools:     m.SupportsTools,
				SupportsReasoning: m.SupportsReasoning,
				InputModalities:   m.InputModalities,
				WebSearch:         m.WebSearch,
				InputPricePerM:    m.InputPricePerM,
				OutputPricePerM:   m.OutputPricePerM,
				Temperature:       m.Temperature,
			})
		}
		out = append(out, t)
	}
	return out
}
