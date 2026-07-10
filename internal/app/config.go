package app

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"code-agent/internal/credential"
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
	// compacts when agent.compact_ratio is unset or invalid. 0.75 sits in the
	// mainstream 70–95% band (Gemini CLI 70%, Codex ≤90%, Claude Code ~92–95%),
	// leaving headroom for output and the compaction pass itself (P12.e).
	defaultCompactRatio = 0.75
	// defaultCompactKeepRatio is the fraction of the compaction threshold kept as
	// the verbatim recent tail when agent.compact_keep_ratio is unset or invalid
	// (P12.a; matches Gemini CLI's 30% preserve fraction).
	defaultCompactKeepRatio = 0.3

	// Provider transport defaults (see ProviderConfig). Tuned for large prompts:
	// prefill on a 90k-token context can exceed a minute, so the per-attempt
	// timeout is generous and transient failures retry with backoff.
	defaultRequestTimeoutSeconds = 120
	defaultMaxRetries            = 2
	defaultBackoffMillis         = 500
	defaultMaxBackoffSeconds     = 8
)

type Config struct {
	DefaultModel string                      `yaml:"default_model"`
	Models       map[string]ModelConfig      `yaml:"models"`
	// Credentials maps namespace → name → config. The outer key is the
	// credential namespace ("gateway", "llm", "mcp"); the inner key is the
	// credential name ("default", "deepseek", "github").
	Credentials  map[string]map[string]CredentialConfig `yaml:"credentials"`
	Agent        AgentConfig                 `yaml:"agent"`
	Provider     ProviderConfig              `yaml:"provider"`

	// Currency is the display symbol for cost reporting (the price fields are in
	// this unit). Defaults to "$".
	Currency string `yaml:"currency"`

	// Web configures the built-in web_search and web_fetch tools. Empty (the
	// default) disables search providers; web_fetch degrades gracefully.
	Web WebConfig `yaml:"web"`

	// MCP configures external Model Context Protocol servers whose tools are
	// registered alongside the built-in ones. It is code-level only (yaml:"-"):
	// MCP servers are configured in a separate Claude-compatible `.mcp.json`
	// document, not in this YAML, so a config authored for Claude Code is consumed
	// verbatim. Desktop entry points populate this from the project-root
	// `.mcp.json` (see mcp.LoadProject); embedded hosts (iOS/macOS) inject it
	// in-memory (see embed.Options.MCPJSON). Empty (the default) disables it.
	MCP mcp.Config `yaml:"-"`

	// Hooks are user-configured pre/post-tool shell commands (8.5). Empty disables.
	Hooks []hooks.Hook `yaml:"hooks"`

	// Permissions pre-approves (or denies) tool calls by name pattern, mirroring
	// Claude Code's permission model — so a user need not confirm every call from a
	// trusted MCP server one at a time. Empty (the default) changes nothing: every
	// side-effecting call still goes through the normal approver. See
	// PermissionsConfig and approve.Allowlisted.
	Permissions PermissionsConfig `yaml:"permissions"`

	// StoreFactory, if set, creates the session store for a workspace root.
	// When nil (default), the built-in SQLite store is used (backward compatible).
	// External consumers that want their own storage backend (e.g. PostgreSQL)
	// set this to their own factory. The returned Store owns its lifecycle;
	// callers must Close it. This field is code-level only (yaml:"-").
	StoreFactory session.StoreFactory `yaml:"-"`

	// GlobalSkillsDir is an optional directory of user-level skills loaded for every
	// workspace. Skills here act as a shared capability pool (always available); a
	// project-local skill of the same name takes precedence. Embedded hosts set it in
	// StartServer from the dataDir parameter. Code-level only (yaml:"-").
	GlobalSkillsDir string `yaml:"-"`

	// Profile selects the platform capability set the runtime assembles for. It is
	// code-level only (set by the embedded host, not the YAML) so a desktop config
	// file can never accidentally downgrade itself. Default (full) assumes a host
	// that can spawn subprocesses and reach the whole filesystem; Sandboxed is for
	// embedded hosts like iOS. See Profile.
	Profile Profile `yaml:"-"`
}

// Profile is the platform capability set the runtime assembles for. The default
// (full) assumes a desktop host. The sandboxed profile is for embedded hosts like
// iOS, where the OS forbids fork/exec and confines the app to its container, so
// every subprocess-based tool (shell, git, gopls, MCP stdio servers, hooks) is
// left unregistered rather than failing at call time.
type Profile string

const (
	// ProfileFull is the default desktop profile: all tools registered.
	ProfileFull Profile = ""
	// ProfileSandboxed omits subprocess-based tools for OS-sandboxed hosts (iOS).
	ProfileSandboxed Profile = "sandboxed"
)

// AllowsSubprocess reports whether the host permits spawning child processes.
// When false, subprocess-based tools and MCP stdio servers are not assembled.
func (p Profile) AllowsSubprocess() bool { return p != ProfileSandboxed }

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

	// Credential explicitly references a credential entry in the credentials
	// section. When set, credential resolution follows this reference instead
	// of using the legacy api_key_env path.
	Credential CredentialRef `yaml:"credential"`

	// Resolved at load time, not read from YAML.
	Name   string `yaml:"-"` // the friendly name (the map key)
	APIKey string `yaml:"-"` // resolved from APIKeyEnv
}

// CredentialRef points to a credential entry in Config.Credentials.
type CredentialRef struct {
	Namespace string `yaml:"namespace"` // "gateway" | "llm" | "mcp"
	Name      string `yaml:"name"`      // "default" | "deepseek" | "github"
}

// IsZero reports whether ref is the zero value.
func (r CredentialRef) IsZero() bool {
	return r.Namespace == "" && r.Name == ""
}

// Target converts the ref to a credential.Target.
func (r CredentialRef) Target() credential.Target {
	return credential.Target{Namespace: r.Namespace, Name: r.Name}
}

// CredentialConfig describes how a named credential is obtained.
type CredentialConfig struct {
	Source string `yaml:"source"` // "env" | "injected" | "none"
	Env    string `yaml:"env"`    // env var name (when source == "env")
}

type AgentConfig struct {
	MaxSteps int `yaml:"max_steps"`

	// VerifyCommand is the project's real build/test command (e.g. "go test ./...").
	// When set, the finalize self-check runs it deterministically once at the end
	// of a turn that changed verifiable code without verifying it (P4.3-R Move 2,
	// the port of Claude Code's Stop hook): a passing run confirms the change, a
	// failing run re-prompts the model with the real failure. Empty (the default)
	// disables the runtime verify — the runtime never guesses "unverified".
	VerifyCommand string `yaml:"verify_command"`

	// CompactRatio is the fraction of a model's context window at which the
	// session compacts. Defaults to defaultCompactRatio; values outside (0,1) are
	// treated as unset.
	CompactRatio float64 `yaml:"compact_ratio"`

	// CompactKeepRatio is the fraction of the compaction threshold kept as the
	// verbatim recent tail when compacting (P12.a). Token-denominated: the tail
	// is CompactThreshold × CompactKeepRatio approximate tokens, which is what
	// makes compaction converge by construction — summary + bounded tail lands
	// back under the threshold on a 32k local window as much as on a 128k one.
	// Defaults to defaultCompactKeepRatio; values outside (0,1) are treated as
	// unset.
	CompactKeepRatio float64 `yaml:"compact_keep_ratio"`

	// SubagentModel names the model a delegated read-only subagent (the `task`
	// tool, 8.3) runs on. Empty inherits the main model; point it at a cheaper
	// model (e.g. a flash-class one) to make read-only investigation cheap. An
	// unknown or key-less name falls back to the main model at runtime.
	SubagentModel string `yaml:"subagent_model"`

	// ClientToolTimeoutSeconds is the lease for a single client-executed tool
	// call (v1.1): how long the loop blocks waiting for the client to deliver a
	// tool_result before giving up with "client timeout". 0 uses the built-in
	// 2-minute default. Raise it when clients run long operations — e.g. a
	// DreamAI sidecar whose generate tool drives image/video generation that
	// routinely exceeds two minutes.
	ClientToolTimeoutSeconds int `yaml:"client_tool_timeout_seconds"`

	// MaxParallelTools caps how many independent, read-only tool calls in one
	// batch execute concurrently (P8.8). 0/1 keeps the strictly sequential loop
	// (the default). Raising it lets the model fan out — e.g. 5 `task` subagents
	// in one turn run at once. Side-effecting calls are always serialized.
	MaxParallelTools int `yaml:"max_parallel_tools"`

	// BuiltinTools, when non-nil, is a deny-by-default allowlist of built-in tool
	// names to register: only the named tools are exposed to the model; everything
	// else (shell, filesystem, git, project_graph, plan_workflow, task, MCP, …) is
	// left out. When nil/unset, every tool registers (the default, unchanged
	// behavior). An empty list registers no built-ins at all.
	//
	// Use it to lock down a deployment that must NOT expose codeagentd's server-side
	// shell/filesystem to end users — e.g. the DreamAI sidecar, whose only needed
	// tool (dreamai_generate) is registered at runtime over the wire, not as a
	// built-in. Set `builtin_tools: []` (or `[web_search, web_fetch]`) there.
	BuiltinTools *[]string `yaml:"builtin_tools"`
}

// ToolAllowed reports whether a tool may be registered. Nil BuiltinTools means
// "no restriction" (all tools allowed). A non-nil list is a deny-by-default
// allowlist: only the named tools are allowed.
func (c AgentConfig) ToolAllowed(name string) bool {
	if c.BuiltinTools == nil {
		return true
	}
	for _, n := range *c.BuiltinTools {
		if n == name {
			return true
		}
	}
	return false
}

// PermissionsConfig holds tool-name glob patterns that pre-approve or deny tool
// calls without a prompt, in Claude Code's `permissions` style. Patterns match a
// tool's model-facing name (e.g. "mcp__github__*", "mcp__db__query", or a
// built-in like "run_command"); '*' is a wildcard. Deny takes precedence over
// allow. A call matching neither list falls through to the normal approver.
//
// This gates only calls that reach the approver — all MCP tools plus
// side-effecting built-ins. Read-only built-ins are never gated, so listing one
// under Deny has no effect.
type PermissionsConfig struct {
	Allow []string `yaml:"allow"` // auto-approve without a prompt
	Deny  []string `yaml:"deny"`  // refuse without a prompt (wins over allow)
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

	// Resolved at load time or injected by a host (e.g. iOS Keychain), not read
	// from YAML. Same pattern as ModelConfig.APIKey.
	BraveKey  string `yaml:"-"`
	TavilyKey string `yaml:"-"`
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
// A directly-set key (injected by a host from a keychain, or set during config
// normalization) takes precedence over the environment variable.
func (c WebSearchConfig) BraveAPIKey() string {
	if c.BraveKey != "" {
		return c.BraveKey
	}
	if c.BraveAPIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.BraveAPIKeyEnv)
}

// TavilyAPIKey returns the resolved Tavily API key, if configured.
// A directly-set key (injected by a host from a keychain, or set during config
// normalization) takes precedence over the environment variable.
func (c WebSearchConfig) TavilyAPIKey() string {
	if c.TavilyKey != "" {
		return c.TavilyKey
	}
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
	var data []byte
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			data = b
		} else if !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
	}
	return LoadConfigBytes(data)
}

// LoadConfigBytes parses configuration from raw YAML bytes (nil or empty =>
// built-in defaults), applying the same normalization and validation as
// LoadConfig. Embedded hosts (iOS/macOS in-app) supply config in-memory rather
// than from a file path, since the app sandbox has no fixed config.yaml.
func LoadConfigBytes(data []byte) (Config, error) {
	cfg := Config{
		Agent:     AgentConfig{MaxSteps: 8},
	}

	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
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
		// Resolve the API key from the env (legacy path). Done first so the
		// credential-ref normalisation below can inspect the resolved key.
		// Injection-priority: a key already set on the struct (e.g. injected by
		// an embedded host from the iOS Keychain) wins over the env lookup.
		if mc.APIKey == "" && mc.APIKeyEnv != "" {
			mc.APIKey = os.Getenv(mc.APIKeyEnv)
		}

		// Normalise credential ref: if not explicitly set, derive from the
		// resolved state. Only derive when we actually have a working credential
		// so SelectModel can still give an early error for missing keys.
		if mc.Credential.IsZero() {
			if mc.APIKey != "" {
				mc.Credential = CredentialRef{Namespace: "llm", Name: name}
			} else if model.IsLocalBaseURL(mc.BaseURL) {
				mc.Credential = CredentialRef{} // none needed
			}
		}
		cfg.Models[name] = mc
	}

	if cfg.Agent.MaxSteps <= 0 {
		cfg.Agent.MaxSteps = 8
	}
	if cfg.Agent.CompactRatio <= 0 || cfg.Agent.CompactRatio >= 1 {
		cfg.Agent.CompactRatio = defaultCompactRatio
	}
	if cfg.Agent.CompactKeepRatio <= 0 || cfg.Agent.CompactKeepRatio >= 1 {
		cfg.Agent.CompactKeepRatio = defaultCompactKeepRatio
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

	// Resolve web search provider keys from the environment — same injection-priority
	// pattern as model keys: a directly-set key (injected by an embedded host from a
	// keychain) wins over the env lookup. On a normal CLI run both are empty here
	// (yaml:"-"), so env resolution is the only path.
	if cfg.Web.Search.TavilyKey == "" && cfg.Web.Search.TavilyAPIKeyEnv != "" {
		cfg.Web.Search.TavilyKey = os.Getenv(cfg.Web.Search.TavilyAPIKeyEnv)
	}
	if cfg.Web.Search.BraveKey == "" && cfg.Web.Search.BraveAPIKeyEnv != "" {
		cfg.Web.Search.BraveKey = os.Getenv(cfg.Web.Search.BraveAPIKeyEnv)
	}

	// MCP servers are not part of this YAML: they are loaded separately from a
	// Claude-compatible `.mcp.json` (see mcp.LoadProject / ParseJSON), which does
	// its own normalization and validation. cfg.MCP is populated by the caller
	// after this returns.

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
	if mc.APIKey == "" && !model.IsLocalBaseURL(mc.BaseURL) && mc.Credential.IsZero() {
		if mc.APIKeyEnv != "" {
			return ModelConfig{}, fmt.Errorf("model %q has no API key; set the %s environment variable", name, mc.APIKeyEnv)
		}
		return ModelConfig{}, fmt.Errorf("model %q has no credential configured; add a credential: section or set api_key_env", name)
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

// CompactKeepTokens is the approximate token budget for the verbatim recent
// tail kept by compaction (P12.a): the compaction threshold scaled by the keep
// ratio. Everything older is folded into the summary.
func (c Config) CompactKeepTokens(mc ModelConfig) int {
	return int(float64(c.CompactThreshold(mc)) * c.Agent.CompactKeepRatio)
}

// CredentialResolver builds a credential.Resolver from the configured
// credentials section and environment. It returns a ChainResolver that tries
// (in order): injected credentials (from the secrets map, populated by the
// caller after LoadConfigBytes), environment variables, and explicit "none".
//
// When the credentials section is empty and no external resolver has been
// injected, a plain EnvResolver is returned (CLI backward compat).
func (c Config) CredentialResolver(injected credential.Resolver) credential.Resolver {
	var resolvers []credential.Resolver

	// 1. Injected credentials (AgentKit secretsJSON / CLI --gateway-token).
	if injected != nil {
		resolvers = append(resolvers, injected)
	}

	// 2. Configured env-based credentials (nested: namespace → name → config).
	if len(c.Credentials) > 0 {
		envResolver := &credential.EnvResolver{}
		for namespace, entries := range c.Credentials {
			for name, cc := range entries {
				if cc.Source == "env" && cc.Env != "" {
					target := credential.Target{Namespace: namespace, Name: name}
					if envResolver.Mapping == nil {
						envResolver.Mapping = make(map[string][]credential.Target)
					}
					envResolver.Mapping[cc.Env] = append(envResolver.Mapping[cc.Env], target)
				}
			}
		}
		if envResolver.Mapping != nil {
			resolvers = append(resolvers, envResolver)
		}
	}

	// 3. Default env resolver for models with api_key_env but no explicit
	//    credential section (backward compat).
	resolvers = append(resolvers, &credential.EnvResolver{})

	return &credential.ChainResolver{Resolvers: resolvers}
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
