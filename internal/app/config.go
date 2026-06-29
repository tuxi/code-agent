package app

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"code-agent/internal/hooks"
	"code-agent/internal/mcp"
	"code-agent/internal/model"
	"code-agent/internal/session"

	"gopkg.in/yaml.v3"
)

const (
	// defaultContextWindow is assumed for any model that does not declare its own
	// context_window. 128k matches the smaller models currently configured.
	defaultContextWindow = 128000
	// defaultCompactRatio is the fraction of the context window at which a session
	// compacts when agent.compact_ratio is unset or invalid.
	defaultCompactRatio = 0.5

	// Provider transport defaults (see ProviderConfig). Tuned for large prompts:
	// prefill on a 90k-token context can exceed a minute, so the per-attempt
	// timeout is generous and transient failures retry with backoff.
	defaultRequestTimeoutSeconds = 120
	defaultMaxRetries            = 2
	defaultBackoffMillis         = 500
	defaultMaxBackoffSeconds     = 8
)

type Config struct {
	DefaultModel string                 `yaml:"default_model"`
	Models       map[string]ModelConfig `yaml:"models"`
	Agent        AgentConfig            `yaml:"agent"`
	Workspace    WorkspaceConfig        `yaml:"workspace"`
	Provider     ProviderConfig         `yaml:"provider"`

	// Currency is the display symbol for cost reporting (the price fields are in
	// this unit). Defaults to "$".
	Currency string `yaml:"currency"`

	// Web configures the built-in web_search and web_fetch tools. Empty (the
	// default) disables search providers; web_fetch degrades gracefully.
	Web WebConfig `yaml:"web"`

	// MCP configures external Model Context Protocol servers whose tools are
	// registered alongside the built-in ones. Empty (the default) disables it.
	MCP mcp.Config `yaml:"mcp"`

	// Hooks are user-configured pre/post-tool shell commands (8.5). Empty disables.
	Hooks []hooks.Hook `yaml:"hooks"`

	// StoreFactory, if set, creates the session store for a workspace root.
	// When nil (default), the built-in SQLite store is used (backward compatible).
	// External consumers that want their own storage backend (e.g. PostgreSQL)
	// set this to their own factory. The returned Store owns its lifecycle;
	// callers must Close it. This field is code-level only (yaml:"-").
	StoreFactory session.StoreFactory `yaml:"-"`
}

// ProviderConfig tunes the transport resilience layer (ResilientProvider):
// per-attempt timeout, retry count, and backoff. Durations are expressed in
// plain integer units so the YAML stays simple.
type ProviderConfig struct {
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"` // per-attempt deadline
	MaxRetries            int `yaml:"max_retries"`             // retries after the first attempt
	BackoffMillis         int `yaml:"backoff_millis"`          // base backoff before the first retry
	MaxBackoffSeconds     int `yaml:"max_backoff_seconds"`     // cap on a single backoff
}

type ModelConfig struct {
	Provider    string  `yaml:"provider"`    // "openai" (openai-compatible); future: anthropic, gemini, ...
	BaseURL     string  `yaml:"base_url"`    // API base URL
	Model       string  `yaml:"model"`       // the wire model string sent to the provider
	APIKeyEnv   string  `yaml:"api_key_env"` // name of the env var holding the API key
	Temperature float64 `yaml:"temperature"` // optional; defaults to 0.2

	// ContextWindow is the model's maximum context in tokens. It sizes the
	// compaction threshold (see Config.CompactThreshold). Defaults to
	// defaultContextWindow when unset.
	ContextWindow int `yaml:"context_window"`

	// InputPricePerM / OutputPricePerM are the price per 1,000,000 prompt and
	// completion tokens, in Config.Currency. Optional; 0 means "unpriced" (cost
	// reporting shows the tokens but no money for this model).
	InputPricePerM  float64 `yaml:"input_price_per_million"`
	OutputPricePerM float64 `yaml:"output_price_per_million"`

	// CacheInputPricePerM is the (lower) price per 1,000,000 prompt tokens served
	// from the provider's prompt cache. Optional; when 0, cached tokens are billed
	// at InputPricePerM (the conservative pre-cache estimate), so cost reporting
	// never silently under-counts a model whose cache price is unconfigured.
	CacheInputPricePerM float64 `yaml:"cache_input_price_per_million"`

	// Resolved at load time, not read from YAML.
	Name   string `yaml:"-"` // the friendly name (the map key)
	APIKey string `yaml:"-"` // resolved from APIKeyEnv
}

type AgentConfig struct {
	MaxSteps int `yaml:"max_steps"`

	// CompactRatio is the fraction of a model's context window at which the
	// session compacts. Defaults to defaultCompactRatio; values outside (0,1) are
	// treated as unset.
	CompactRatio float64 `yaml:"compact_ratio"`

	// SubagentModel names the model a delegated read-only subagent (the `task`
	// tool, 8.3) runs on. Empty inherits the main model; point it at a cheaper
	// model (e.g. a flash-class one) to make read-only investigation cheap. An
	// unknown or key-less name falls back to the main model at runtime.
	SubagentModel string `yaml:"subagent_model"`
}

type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

// WebConfig configures the built-in web_search and web_fetch tools. When empty,
// the tools degrade gracefully: web_search returns an error advising the user to
// configure a search provider, and web_fetch still fetches URLs but without
// caching (since no cache TTL is configured).
type WebConfig struct {
	Search WebSearchConfig `yaml:"search"`
	Fetch  WebFetchConfig  `yaml:"fetch"`
}

type WebSearchConfig struct {
	Provider         string `yaml:"provider"`           // "tavily" (default), "brave", or "searxng"
	FallbackProvider string `yaml:"fallback_provider"`  // optional fallback
	SearXNGBaseURL   string `yaml:"searxng_base_url"`   // SearXNG instance base URL (single or comma-separated)
	BraveAPIKeyEnv   string `yaml:"brave_api_key_env"`  // env var holding Brave API key
	TavilyAPIKeyEnv  string `yaml:"tavily_api_key_env"` // env var holding Tavily API key
	TopK             int    `yaml:"top_k"`              // max results, default 5
	TimeoutSeconds   int    `yaml:"timeout_seconds"`    // HTTP timeout, default 10
}

// SearXNGInstances returns the list of SearXNG instances from config.
// If searxng_base_url is set, it is split on commas to form the list.
// Otherwise the built-in defaults are used.
func (c WebSearchConfig) SearXNGInstances() []string {
	if c.SearXNGBaseURL != "" {
		parts := strings.Split(c.SearXNGBaseURL, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	return nil // caller uses defaults
}

// BraveAPIKey returns the resolved Brave API key, if configured.
func (c WebSearchConfig) BraveAPIKey() string {
	if c.BraveAPIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.BraveAPIKeyEnv)
}

// TavilyAPIKey returns the resolved Tavily API key, if configured.
func (c WebSearchConfig) TavilyAPIKey() string {
	if c.TavilyAPIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.TavilyAPIKeyEnv)
}

type WebFetchConfig struct {
	TimeoutSeconds  int `yaml:"timeout_seconds"`   // HTTP timeout, default 30
	CacheTTLSeconds int `yaml:"cache_ttl_seconds"` // URL cache TTL, 0 disables
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		Agent:     AgentConfig{MaxSteps: 8},
		Workspace: WorkspaceConfig{Root: "."},
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return Config{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
	}

	// Fallback: if no models are configured (e.g. an old config or no file),
	// provide a built-in deepseek entry so the tool still works out of the box.
	if len(cfg.Models) == 0 {
		cfg.Models = map[string]ModelConfig{
			"deepseek": {
				Provider:  "openai",
				BaseURL:   "https://api.deepseek.com",
				Model:     "deepseek-v4-flash",
				APIKeyEnv: "DEEPSEEK_API_KEY",
			},
		}
	}

	if cfg.DefaultModel == "" {
		if _, ok := cfg.Models["deepseek"]; ok {
			cfg.DefaultModel = "deepseek"
		} else {
			names := modelNames(cfg.Models)
			cfg.DefaultModel = names[0]
		}
	}

	// Resolve per-model defaults and API keys. Missing keys are NOT an error
	// here; they are reported only when a model is actually selected.
	for name, mc := range cfg.Models {
		mc.Name = name
		if mc.Provider == "" {
			mc.Provider = "openai"
		}
		if mc.Temperature <= 0 {
			mc.Temperature = 0.2
		}
		if mc.ContextWindow <= 0 {
			mc.ContextWindow = defaultContextWindow
		}
		if mc.APIKeyEnv != "" {
			mc.APIKey = os.Getenv(mc.APIKeyEnv)
		}
		cfg.Models[name] = mc
	}

	if cfg.Agent.MaxSteps <= 0 {
		cfg.Agent.MaxSteps = 8
	}
	if cfg.Agent.CompactRatio <= 0 || cfg.Agent.CompactRatio >= 1 {
		cfg.Agent.CompactRatio = defaultCompactRatio
	}

	if cfg.Provider.RequestTimeoutSeconds <= 0 {
		cfg.Provider.RequestTimeoutSeconds = defaultRequestTimeoutSeconds
	}
	if cfg.Provider.MaxRetries <= 0 {
		cfg.Provider.MaxRetries = defaultMaxRetries
	}
	if cfg.Provider.BackoffMillis <= 0 {
		cfg.Provider.BackoffMillis = defaultBackoffMillis
	}
	if cfg.Provider.MaxBackoffSeconds <= 0 {
		cfg.Provider.MaxBackoffSeconds = defaultMaxBackoffSeconds
	}
	if cfg.Currency == "" {
		cfg.Currency = "$"
	}
	if cfg.Workspace.Root == "" {
		cfg.Workspace.Root = "."
	}

	if _, ok := cfg.Models[cfg.DefaultModel]; !ok {
		return Config{}, fmt.Errorf("default_model %q is not defined under models", cfg.DefaultModel)
	}

	if cfg.Web.Search.Provider == "" {
		cfg.Web.Search.Provider = "tavily"
	}
	// SearXNG instances default to the built-in public pool when not configured.
	if cfg.Web.Search.TopK <= 0 {
		cfg.Web.Search.TopK = 5
	}
	if cfg.Web.Search.TimeoutSeconds <= 0 {
		cfg.Web.Search.TimeoutSeconds = 10
	}
	if cfg.Web.Fetch.TimeoutSeconds <= 0 {
		cfg.Web.Fetch.TimeoutSeconds = 30
	}
	if cfg.Web.Fetch.CacheTTLSeconds <= 0 {
		cfg.Web.Fetch.CacheTTLSeconds = 600 // 10 minutes
	}

	// MCP servers: names must be present and unique (they namespace the tools),
	// and a command is required to launch the stdio server. Fail at load with a
	// clear message rather than letting a duplicate name collide at registration.
	seenMCP := make(map[string]bool, len(cfg.MCP.Servers))
	for i, s := range cfg.MCP.Servers {
		switch {
		case s.Name == "":
			return Config{}, fmt.Errorf("mcp.servers[%d]: name is required", i)
		case s.Command == "":
			return Config{}, fmt.Errorf("mcp server %q: command is required", s.Name)
		case seenMCP[s.Name]:
			return Config{}, fmt.Errorf("mcp server %q: duplicate name", s.Name)
		}
		seenMCP[s.Name] = true
	}

	return cfg, nil
}

// SelectModel resolves a model by friendly name (empty name => default_model).
// It fails if the model is unknown or its API key is not set.
func (c Config) SelectModel(name string) (ModelConfig, error) {
	if name == "" {
		name = c.DefaultModel
	}
	mc, ok := c.Models[name]
	if !ok {
		return ModelConfig{}, fmt.Errorf("unknown model %q; configured models: %s",
			name, strings.Join(c.ModelNames(), ", "))
	}
	if mc.APIKey == "" && !model.IsLocalBaseURL(mc.BaseURL) {
		return ModelConfig{}, fmt.Errorf("model %q has no API key; set the %s environment variable",
			name, mc.APIKeyEnv)
	}
	return mc, nil
}

// CompactThreshold is the prompt-token count at which a session running the
// given model should compact: the model's context window scaled by the
// configured compact ratio. This is what makes compaction model-aware — a
// 256k-window model gets a proportionally higher threshold than a 128k one.
func (c Config) CompactThreshold(mc ModelConfig) int {
	return int(float64(mc.ContextWindow) * c.Agent.CompactRatio)
}

// ModelNames returns the configured model names, sorted.
func (c Config) ModelNames() []string {
	return modelNames(c.Models)
}

func modelNames(models map[string]ModelConfig) []string {
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
