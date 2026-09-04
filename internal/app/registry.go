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
// registry is the SINGLE SOURCE OF TRUTH for built-in providers: it fills
// fields ONLY when a model config leaves them unset (explicit base_url /
// api_key_env / context window / pricing / capability in YAML always win), so
// a runtime upgrade updates instance data and capabilities without touching
// persisted settings.

type builtinModelTemplate struct {
	ID                string   // wire model id
	API               string   // openai、responses、claude、ollama，默认为空，使用厂商provider 的配置ProviderType
	RuntimeAlias      string   // short friendly name (optional)
	ContextWindow     int      // token limit
	SupportsTools     bool     // tool calling
	SupportsReasoning bool     // reasoning/thinking
	InputModalities   []string // "text", "image", "audio"
	WebSearch         bool     // supports web search tool
	InputPricePerM    float64  // USD per million input tokens
	OutputPricePerM   float64  // USD per million output tokens
	Temperature       float64  // provider-recommended default (0 = use global default)

	// ReasoningEffort is the model's official default thinking budget
	// ("low"|"medium"|"high"|"x-high"|"max"; "" = provider default). Filled
	// into ModelConfig.ReasoningEffort when the config leaves it empty.
	ReasoningEffort string
	// SupportedReasoningEfforts lists the effort levels the provider's API
	// accepts for this model. Empty = the model has a reasoning toggle but no
	// standardized effort control (the host shows the toggle only).
	SupportedReasoningEfforts []string
	// CanDisableReasoning reports whether reasoning may be turned off entirely
	// (false = reasoner-only; nil = allowed).
	CanDisableReasoning *bool
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

// boolPtr returns a pointer to b — registry literals cannot take the address
// of a constant.
func boolPtr(b bool) *bool { return &b }

var builtinConnections = map[string]builtinConnection{
	"deepseek": {
		BaseURL: "https://api.deepseek.com", Env: "DEEPSEEK_API_KEY",
		WireModel: "deepseek-v4-flash", ProviderType: "openai",
		DisplayName: "DeepSeek", Summary: "使用 DeepSeek API 密钥连接", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "deepseek-v4-flash", RuntimeAlias: "deepseek", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, WebSearch: true, InputPricePerM: 0.16, OutputPricePerM: 0.32, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "low"},
			{ID: "deepseek-v4-pro", RuntimeAlias: "deepseek-pro", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.45, OutputPricePerM: 0.90, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "low"},
			{ID: "deepseek-v4-flash-vision-exp", RuntimeAlias: "deepseek-vision", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text", "image"}, InputPricePerM: 0.16, OutputPricePerM: 0.32, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "low"},
		},
	},
	"qwen": {
		BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Env: "DASHSCOPE_API_KEY",
		WireModel: "qwen3-coder-plus", ProviderType: "openai",
		DisplayName: "Alibaba Qwen", Summary: "使用阿里云百炼 OpenAI 兼容接口", Kind: "api_key",
		Models: []builtinModelTemplate{
			// qwen3-coder thinks via a boolean enable_thinking switch — the
			// DashScope compatible endpoint has no standardized effort levels,
			// so the host shows the reasoning toggle only.
			{ID: "qwen3-coder-plus", ContextWindow: 128_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, CanDisableReasoning: boolPtr(true)},
		},
	},
	"glm": {
		BaseURL: "https://open.bigmodel.cn/api/paas/v4", Env: "GLM_API_KEY",
		WireModel: "glm-4.7", ProviderType: "openai",
		DisplayName: "Zhipu GLM", Summary: "使用智谱 OpenAI 兼容接口", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "glm-5.3-flash", RuntimeAlias: "glm", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text", "image"}, InputPricePerM: 0.16, OutputPricePerM: 0.32, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "medium"},
		},
	},
	"openrouter": {
		BaseURL: "https://openrouter.ai/api/v1", Env: "OPENROUTER_API_KEY",
		ProviderType: "openai",
		DisplayName:  "OpenRouter", Summary: "通过一个 API 密钥使用多个模型", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "openrouter/auto", RuntimeAlias: "openrouter", ContextWindow: 200_000, SupportsTools: true, InputModalities: []string{"text"}},
			{ID: "openrouter/free", RuntimeAlias: "openrouter", ContextWindow: 200_000, SupportsTools: true, InputModalities: []string{"text"}},
		},
	},
	"bai": {
		BaseURL: "https://api.b.ai/v1/", Env: "BAI_API_KEY",
		ProviderType: "openai",
		DisplayName:  "B.ai", Summary: "通过一个 API 密钥使用多个模型", Kind: "api_key",
		Models: []builtinModelTemplate{
			{ID: "deepseek-v4-flash", RuntimeAlias: "deepseek", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, WebSearch: true, InputPricePerM: 0.16, OutputPricePerM: 0.32, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "low"},
			{ID: "deepseek-v4-pro", RuntimeAlias: "deepseek-pro", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.45, OutputPricePerM: 0.90, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "low"},
			{ID: "deepseek-v4-flash-vision-exp", RuntimeAlias: "deepseek-vision", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text", "image"}, InputPricePerM: 0.16, OutputPricePerM: 0.32, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "low"},
			{ID: "glm-5.3-flash", RuntimeAlias: "glm-flash", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text", "image"}, InputPricePerM: 0.16, OutputPricePerM: 0.32, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "medium"},
			{ID: "mimo-v2.5", RuntimeAlias: "mino-v2.5", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.16, OutputPricePerM: 0.32, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "medium"},
			{ID: "qwen3.8-flash", RuntimeAlias: "qwen3.8-flash", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.16, OutputPricePerM: 0.32, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "medium"},
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
			{ID: "gpt-5.6-luna", RuntimeAlias: "opencode gpt 5.6 luna", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.22, OutputPricePerM: 0.66, SupportedReasoningEfforts: []string{"low", "medium", "high", "x-high", "max"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "medium"},
			{ID: "deepseek-v4-flash", RuntimeAlias: "opencode deepseek flash", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.22, OutputPricePerM: 0.66, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "low"},
			{ID: "deepseek-v4-pro", RuntimeAlias: "opencode deepseek pro", ContextWindow: 1_000_000, SupportsTools: true, SupportsReasoning: true, InputModalities: []string{"text"}, InputPricePerM: 0.66, OutputPricePerM: 1.98, SupportedReasoningEfforts: []string{"low", "medium", "high"}, CanDisableReasoning: boolPtr(true), ReasoningEffort: "low"},
			{ID: "kimi-k3", RuntimeAlias: "opencode kimi k3", ContextWindow: 1_000_000, SupportsTools: true, InputModalities: []string{"text"}, InputPricePerM: 3.00, OutputPricePerM: 15.00, Temperature: 1.0},
			{ID: "kimi-k2.7-code", RuntimeAlias: "opencode kimi k2.7 code", ContextWindow: 256_000, SupportsTools: true, InputModalities: []string{"text"}, InputPricePerM: 1.2, OutputPricePerM: 5.6, Temperature: 1.0},
			{ID: "glm-5.2", RuntimeAlias: "opencode glm 5.2", ContextWindow: 128_000, SupportsTools: true, InputModalities: []string{"text"}, InputPricePerM: 1.40, OutputPricePerM: 4.40, Temperature: 0.2},
			{ID: "mimo-v2.5", RuntimeAlias: "opencode mimo v2.5", ContextWindow: 1_000_000, SupportsTools: true, InputModalities: []string{"text"}, InputPricePerM: 0.22, OutputPricePerM: 0.66, Temperature: 0.2},
		},
	},
}

// applyRegistryDefaults fills unset BaseURL/APIKeyEnv from the built-in
// connection registry. The connection is resolved by the model's friendly
// name, falling back to Catalog.ConnectionID so grouped-provider models
// (whose names are alias/friendly keys, not connection ids) get the same
// official capability defaults. Explicit values in the config are never
// overwritten.
//
// design-provider-id-model.md: when the user writes a known service id (e.g.
// "provider: deepseek" with a model also named deepseek), the registry also
// resolves the service id to its api type (ProviderType) and records the
// service id in Catalog.ProviderID for /v1/runtime/models. A generic api type
// (openai/responses/ollama) or an explicit base_url is never overwritten.
func applyRegistryDefaults(mc *settings.ModelConfig) {
	connID := mc.Name
	if mc.Catalog.ConnectionID != "" {
		connID = mc.Catalog.ConnectionID
	}
	conn, ok := builtinConnections[connID]
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
	// Per-model defaults: temperature (some providers, e.g. OpenCode Go /
	// Kimi K3, reject the global default of 0.2 and require a specific value)
	// and the official reasoning capability — default effort, supported
	// effort levels, and whether reasoning can be turned off. Registry values
	// fill ONLY unset fields; an explicit config value always wins.
	if mc.Model != "" && len(conn.Models) > 0 {
		for _, m := range conn.Models {
			if m.ID != mc.Model {
				continue
			}
			if mc.Temperature <= 0 && m.Temperature > 0 {
				mc.Temperature = m.Temperature
			}
			if mc.ContextWindow <= 0 && m.ContextWindow > 0 {
				mc.ContextWindow = m.ContextWindow
			}
			if mc.InputPricePerM == 0 && m.InputPricePerM > 0 {
				mc.InputPricePerM = m.InputPricePerM
			}
			if mc.OutputPricePerM == 0 && m.OutputPricePerM > 0 {
				mc.OutputPricePerM = m.OutputPricePerM
			}
			// Web search is a plain bool (no unset state): fill it on when the
			// registry declares it, never force it off.
			if !mc.WebSearch && m.WebSearch {
				mc.WebSearch = true
			}
			if mc.ReasoningEffort == "" {
				mc.ReasoningEffort = m.ReasoningEffort
			}
			if len(mc.Catalog.SupportedReasoningEfforts) == 0 {
				mc.Catalog.SupportedReasoningEfforts = m.SupportedReasoningEfforts
			}
			if mc.Catalog.CanDisableReasoning == nil {
				mc.Catalog.CanDisableReasoning = m.CanDisableReasoning
			}
			if mc.Catalog.SupportsReasoning == nil && m.SupportsReasoning {
				mc.Catalog.SupportsReasoning = boolPtr(true)
			}
			break
		}
	}
}

// BuiltinProviderModelIDs returns the suggested model ids for a known
// connection — the payload PUT /v1/providers/{id} needs when a client
// connects a built-in provider with ONLY an API key: the server fills the
// model list from the registry, so the registry stays the single source of
// truth (WorkBuddy/OpenCode-style api-key-only onboarding).
//
// Persisted entries carry ids ONLY; instance data (context window, pricing,
// temperature) and capabilities (tool calling, reasoning, modalities) are
// filled at expansion time by applyRegistryDefaults from the same registry,
// so a runtime upgrade updates everything without touching settings.json.
//
// ok=false for unknown (custom) ids. ids=false (empty, ok=true) for known
// connections without suggested models (ollama) — those still require the
// client to declare models explicitly.
func BuiltinProviderModelIDs(id string) (ids []string, ok bool) {
	conn, known := builtinConnections[id]
	if !known {
		return nil, false
	}
	for _, m := range conn.Models {
		ids = append(ids, m.ID)
	}
	return ids, true
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
	API               string
	RuntimeAlias      string
	ContextWindow     int
	SupportsTools     bool
	SupportsReasoning bool
	InputModalities   []string
	WebSearch         bool
	InputPricePerM    float64
	OutputPricePerM   float64
	Temperature       float64

	// ReasoningEffort is the model's official default thinking budget
	// ("" = provider default).
	ReasoningEffort string
	// SupportedReasoningEfforts lists the effort levels the provider's API
	// accepts (empty = toggle only, no standardized effort control).
	SupportedReasoningEfforts []string
	// CanDisableReasoning reports whether reasoning may be turned off entirely
	// (nil = allowed).
	CanDisableReasoning *bool
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
				ID:                        m.ID,
				API:                       m.API,
				RuntimeAlias:              m.RuntimeAlias,
				ContextWindow:             m.ContextWindow,
				SupportsTools:             m.SupportsTools,
				SupportsReasoning:         m.SupportsReasoning,
				InputModalities:           m.InputModalities,
				WebSearch:                 m.WebSearch,
				InputPricePerM:            m.InputPricePerM,
				OutputPricePerM:           m.OutputPricePerM,
				Temperature:               m.Temperature,
				ReasoningEffort:           m.ReasoningEffort,
				SupportedReasoningEfforts: m.SupportedReasoningEfforts,
				CanDisableReasoning:       m.CanDisableReasoning,
			})
		}
		out = append(out, t)
	}
	return out
}
