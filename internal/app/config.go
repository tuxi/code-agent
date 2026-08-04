package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	DefaultModel string                 `yaml:"default_model"`
	Models       map[string]ModelConfig `yaml:"models"`
	// Credentials maps namespace → name → config. The outer key is the
	// credential namespace ("gateway", "llm", "mcp"); the inner key is the
	// credential name ("default", "deepseek", "github").
	Credentials map[string]map[string]CredentialConfig `yaml:"credentials"`
	Agent       AgentConfig                            `yaml:"agent"`
	Provider    ProviderConfig                         `yaml:"provider"`
	Runtime     RuntimeConfig                          `yaml:"runtime"`
	Server      ServerConfig                           `yaml:"server"`

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
	// verbatim. CLI entry points (run/repl/tui) populate this from CWD's
	// `.mcp.json` via mcp.ResolveDesktop; the daemon (codeagentd) leaves it empty
	// and resolves MCP per conversation workspace via WorkspaceRegistry.
	// Embedded hosts (iOS/macOS) inject it in-memory (see embed.Options.MCPJSON).
	// Empty (the default) disables it.
	MCP mcp.Config `yaml:"-"`

	// Hooks are user-configured pre/post-tool shell commands (8.5). Empty disables.
	Hooks []hooks.Hook `yaml:"hooks"`

	// Permissions pre-approves (or denies) tool calls by name pattern, mirroring
	// Claude Code's permission model — so a user need not confirm every call from a
	// trusted MCP server one at a time. Empty (the default) changes nothing: every
	// side-effecting call still goes through the normal approver. See
	// PermissionsConfig and approve.Allowlisted.
	Permissions PermissionsConfig `yaml:"permissions"`

	// Warnings collects non-fatal configuration notes raised during load (e.g.
	// legacy api_key_env usage slated for removal). Printed by CLI entry points
	// at startup; empty when the config is clean. Code-level only (yaml:"-").
	Warnings []string `yaml:"-"`

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

	// GlobalPromptsDir is an optional directory of user-level prompt templates.
	// Same pattern as GlobalSkillsDir: embedded hosts set it from dataDir; desktop
	// defaults to ~/.codeagent/prompts/.
	GlobalPromptsDir string `yaml:"-"`

	// Profile selects the platform capability set the runtime assembles for. It is
	// code-level only (set by the embedded host, not the YAML) so a desktop config
	// file can never accidentally downgrade itself. Default (full) assumes a host
	// that can spawn subprocesses and reach the whole filesystem; Sandboxed is for
	// embedded hosts like iOS. See Profile.
	Profile Profile `yaml:"-"`
}

// RuntimeConfig controls process-wide turn admission. A non-positive value is
// normalized to one so older configs retain the safe FIFO behavior.
type RuntimeConfig struct {
	MaxConcurrentTurns int `yaml:"max_concurrent_turns"`
}

// ServerConfig configures an independently running Runtime Server. Embedded
// hosts ignore authentication and TLS fields and inject an in-memory token
// directly through embed.Options.
type ServerConfig struct {
	DisplayName    string `yaml:"display_name"`
	Authentication string `yaml:"authentication"` // none | bearer
	AccessToken    string `yaml:"access_token"`
	AccessTokenEnv string `yaml:"access_token_env"`
	PublicHealthz  bool   `yaml:"public_healthz"`
	TLSCertificate string `yaml:"tls_certificate"`
	TLSPrivateKey  string `yaml:"tls_private_key"`
}

func (c Config) RuntimeMaxConcurrentTurns() int {
	if c.Runtime.MaxConcurrentTurns < 1 {
		return 1
	}
	return c.Runtime.MaxConcurrentTurns
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
	Credential CredentialRef        `yaml:"credential"`
	Catalog    ModelCatalogMetadata `yaml:"catalog"`

	// Resolved at load time, not read from YAML.
	Name string `yaml:"-"` // the friendly name (the map key)
}

// ModelCatalogMetadata is optional, non-secret presentation metadata used by
// /v1/runtime/models. It does not participate in provider routing.
type ModelCatalogMetadata struct {
	ConnectionID          string   `yaml:"connection_id"`
	ProviderID            string   `yaml:"provider_id"`
	ConnectionDisplayName string   `yaml:"connection_display_name"`
	DisplayName           string   `yaml:"display_name"`
	SupportsTools         *bool    `yaml:"supports_tools"`
	SupportsReasoning     bool     `yaml:"supports_reasoning"`
	InputModalities       []string `yaml:"input_modalities"`
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

// WebConfig configures the built-in web_search and web_fetch tools. When search
// is empty or disabled, web_search is not registered; web_fetch remains
// available.
type WebConfig struct {
	Search WebSearchConfig `yaml:"search"`
	Fetch  WebFetchConfig  `yaml:"fetch"`
}

type WebSearchConfig struct {
	Provider              string        `yaml:"provider"`                // "tavily", "gateway", "brave", "searxng", or "disabled"
	FallbackProvider      string        `yaml:"fallback_provider"`       // optional fallback
	GatewayBaseURL        string        `yaml:"gateway_base_url"`        // Agent Gateway /api/v1/agent base URL
	GatewayTimeoutSeconds int           `yaml:"gateway_timeout_seconds"` // whole managed search request, default 120
	Credential            CredentialRef `yaml:"credential"`              // managed search credential; defaults to gateway/default
	SearXNGBaseURL        string        `yaml:"searxng_base_url"`        // SearXNG instance base URL (single or comma-separated)
	BraveAPIKeyEnv        string        `yaml:"brave_api_key_env"`       // env var holding Brave API key
	TavilyAPIKeyEnv       string        `yaml:"tavily_api_key_env"`      // env var holding Tavily API key
	TopK                  int           `yaml:"top_k"`                   // max results, default 5
	TimeoutSeconds        int           `yaml:"timeout_seconds"`         // HTTP timeout, default 10

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

// LoadConfigLayered loads configuration with the flattening layering
// (design-connection-flattening §8.3): built-in registry defaults (layer 1,
// applied during LoadConfigBytes normalization) → user-global
// ~/.codeagent/config.yaml (layer 2) → project <cwd>/.codeagent/config.yaml
// (layer 3). The project layer wins on conflict; a missing user file is not
// an error.
//
// The project config lives under a hidden directory (.codeagent/) to avoid
// colliding with any project's own config.yaml (§8.5.1). When the new path
// does not exist but an old bare config.yaml does, the old file is read with
// a deprecation warning.
//
// LoadConfig keeps its single-file semantics for embedded hosts and tests;
// CLI/daemon entry points use this layered form so model/credential config
// follows the user across directories instead of requiring a config.yaml copy
// in every workspace.
func LoadConfigLayered(projectPath string) (Config, error) {
	// Layer 2: user-global config.
	var userCfg Config
	if home, err := os.UserHomeDir(); err == nil {
		userPath := filepath.Join(home, ".codeagent", "config.yaml")
		if data, err := os.ReadFile(userPath); err == nil {
			if userCfg, err = LoadConfigBytes(data); err != nil {
				return Config{}, fmt.Errorf("user config %s: %w", userPath, err)
			}
		} else if errors.Is(err, os.ErrNotExist) {
			bootstrapUserConfig(userPath)
		} else {
			return Config{}, fmt.Errorf("read user config %s: %w", userPath, err)
		}
	}

	// Layer 3: project config (<cwd>/.codeagent/config.yaml).
	projectCfg, err := LoadConfig(projectPath)
	if err != nil {
		return Config{}, err
	}
	// When the new project path does not exist, try the legacy bare
	// config.yaml (with warning) before falling back to user config alone.
	// This avoids the conflict with backend projects' own config.yaml and
	// the "default_model is not defined" error when a bare project file
	// has no models.
	if projectFileDidNotExist(projectPath) {
		if oldCfg, oldErr := tryLegacyProjectConfig(); oldErr == nil {
			oldCfg.Warnings = append(oldCfg.Warnings,
				"config.yaml at the project root is deprecated; move it to .codeagent/config.yaml")
			if userCfg.DefaultModel != "" && oldCfg.DefaultModel == "" {
				oldCfg.DefaultModel = userCfg.DefaultModel
			}
			merged := MergeConfigs(userCfg, oldCfg)
			if err := validateMergedDefaultModel(merged); err != nil {
				return Config{}, err
			}
			return merged, nil
		}
		if err := validateMergedDefaultModel(userCfg); err != nil {
			return Config{}, err
		}
		return userCfg, nil
	}
	merged := MergeConfigs(userCfg, projectCfg)
	if err := validateMergedDefaultModel(merged); err != nil {
		return Config{}, err
	}
	return merged, nil
}

// validateMergedDefaultModel checks that the default_model (after merging
// all layers) is present in the merged model space. This replaces the
// single-file validation that was in LoadConfigBytes; layered loads must
// validate against the fully merged result because the project layer may
// have default_model while models come from the user layer.
func validateMergedDefaultModel(cfg Config) error {
	if len(cfg.Models) == 0 && cfg.DefaultModel != "" {
		return fmt.Errorf("default_model %q cannot be set when models is empty", cfg.DefaultModel)
	}
	if len(cfg.Models) > 0 && cfg.DefaultModel != "" {
		if _, ok := cfg.Models[cfg.DefaultModel]; !ok {
			return fmt.Errorf("default_model %q is not defined under models", cfg.DefaultModel)
		}
	}
	return nil
}

// tryLegacyProjectConfig reads a bare config.yaml in the current directory
// for backward compatibility. A nonexistent file returns an error.
func tryLegacyProjectConfig() (Config, error) {
	return LoadConfig("config.yaml")
}

// projectFileDidNotExist reports whether the project config path was absent
// when LoadConfig was called — the config came entirely from builtin defaults.
func projectFileDidNotExist(path string) bool {
	if path == "" {
		return true
	}
	_, err := os.Stat(path)
	return err != nil
}

// MergeConfigs merges a lower-priority (user-global) config with a
// higher-priority (project) config. The flattening-relevant sections are
// merged per design §8.3: models and credentials union with the project layer
// winning per key; default_model and subagent_model fall back to the user
// layer when the project leaves them unset. All other sections (server,
// provider, agent, web, ...) come from the project layer, which is already
// defaulted by LoadConfigBytes — a user config that does not touch them has no
// effect, matching the "project owns behavior, user owns models/credentials"
// split.
func MergeConfigs(user, project Config) Config {
	// Models: union, project wins per friendly name.
	if project.Models == nil {
		project.Models = map[string]ModelConfig{}
	}
	for name, mc := range user.Models {
		if _, exists := project.Models[name]; !exists {
			project.Models[name] = mc
		}
	}
	// Credentials: union, project wins per namespace/name.
	if project.Credentials == nil {
		project.Credentials = map[string]map[string]CredentialConfig{}
	}
	for ns, entries := range user.Credentials {
		merged, ok := project.Credentials[ns]
		if !ok {
			merged = map[string]CredentialConfig{}
		}
		for name, cc := range entries {
			if _, exists := merged[name]; !exists {
				merged[name] = cc
			}
		}
		project.Credentials[ns] = merged
	}
	// default_model / subagent_model: project wins when set, else user.
	if project.DefaultModel == "" {
		project.DefaultModel = user.DefaultModel
	}
	// Agent 段字段级合并（项目 wins when set, else user）——agent 性能参数
	// （max_steps/max_parallel_tools/compact_ratio/compact_keep_ratio/
	// client_tool_timeout_seconds）是用户级偏好，项目未显式设置时应继承用户。
	// 0 是 LoadConfigBytes 归一化后的「未设置」哨兵值，字段级覆盖安全。
	mergeAgentField := func(dst *int, src int) {
		if *dst == 0 {
			*dst = src
		}
	}
	mergeAgentField(&project.Agent.MaxSteps, user.Agent.MaxSteps)
	mergeAgentField(&project.Agent.MaxParallelTools, user.Agent.MaxParallelTools)
	mergeAgentField(&project.Agent.ClientToolTimeoutSeconds, user.Agent.ClientToolTimeoutSeconds)
	if project.Agent.CompactRatio <= 0 || project.Agent.CompactRatio >= 1 {
		project.Agent.CompactRatio = user.Agent.CompactRatio
	}
	if project.Agent.CompactKeepRatio <= 0 || project.Agent.CompactKeepRatio >= 1 {
		project.Agent.CompactKeepRatio = user.Agent.CompactKeepRatio
	}
	if project.Agent.SubagentModel == "" {
		project.Agent.SubagentModel = user.Agent.SubagentModel
	}

	// Provider 段字段级合并（超时/重试/退避同样按「项目 wins when set, else user」）。
	mergeProviderField := func(dst *int, src int) {
		if *dst == 0 {
			*dst = src
		}
	}
	mergeProviderField(&project.Provider.RequestTimeoutSeconds, user.Provider.RequestTimeoutSeconds)
	mergeProviderField(&project.Provider.MaxRetries, user.Provider.MaxRetries)
	mergeProviderField(&project.Provider.BackoffMillis, user.Provider.BackoffMillis)
	mergeProviderField(&project.Provider.MaxBackoffSeconds, user.Provider.MaxBackoffSeconds)
	// Merge warnings from both layers.
	project.Warnings = append(project.Warnings, user.Warnings...)
	return project
}

// bootstrapUserConfig writes a commented template config to userPath on first
// launch so the user knows the file exists and can edit it. The built-in
// registry already provides open-box models; this file is a scaffold (nil return
// = start without user config, same as before). Failure is best-effort — the
// registry keeps the runtime usable.
//
// The template is deliberately comprehensive: every config section is present,
// commented, with the built-in default value shown, so a user can uncomment the
// knob they care about without hunting through docs. Unknown/unsupported
// sections are intentionally omitted (hooks, permissions — those live in the
// project settings layer, P11).
func bootstrapUserConfig(userPath string) {
	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		return
	}
	const template = `# ~/.codeagent/config.yaml — 用户级配置（任何目录启动均生效）
# 首次启动时自动生成。取消注释想要覆盖的项即可；未覆盖项使用内建默认值。
# 内建 registry 已提供 deepseek / qwen / glm / ollama / gateway 的开箱模型，
# 本文件的 models / credentials 在其基础上追加或覆盖。
#
# 分层：用户级（本文件）→ 项目级（<cwd>/.codeagent/config.yaml）。
# 项目文件存在时，项目未显式设置的字段继承本文件的用户级值。

# ── 默认模型 ────────────────────────────────────────────────
# default_model: deepseek-pro        # 未设置时用 deepseek / 第一个模型

# ── 模型选择（子代理 / 成本偏好）─────────────────────────────
# subagent_model: deepseek           # task 子代理用哪个模型

# ── Agent 行为（性能偏好）────────────────────────────────────
# agent:
#   max_steps: 68                    # 单个任务最大步数（默认 8）
#   max_parallel_tools: 8            # 并行工具数（0/1 = 严格串行）
#   compact_ratio: 0.75              # 上下文压缩阈值（默认 0.75）
#   compact_keep_ratio: 0.3          # 压缩保留的最近尾部比例
#   client_tool_timeout_seconds: 900 # 客户端工具执行超时
#   subagent_model: deepseek         # 或在此处设置子代理模型

# ── Provider 传输层（超时 / 重试）─────────────────────────────
# provider:
#   request_timeout_seconds: 600     # 单次请求超时
#   max_retries: 5                   # 重试次数
#   backoff_millis: 500              # 首次退避
#   max_backoff_seconds: 8           # 退避上限

# ── 凭证来源 ────────────────────────────────────────────────
# credentials:
#   llm:
#     my-key:                        # 直连模型 API key
#       source: env                  # env | injected | none
#       env: MY_API_KEY              # source=env 时的环境变量名
#   gateway:
#     default:
#       source: injected             # gateway JWT（宿主注入）

# ── 自定义模型（registry 之外 / 覆盖）────────────────────────
# models:
#   my-model:
#     provider: openai               # openai | ollama
#     base_url: https://api.example.com/v1
#     model: my-model-name
#     credential:
#       namespace: llm               # llm | gateway
#       name: my-key                 # 引用上方 credentials 条目
#     context_window: 128000
#     input_price_per_million: 0.27
#     output_price_per_million: 1.10

# ── Web 搜索 / 抓取 ──────────────────────────────────────────
# web:
#   search:
#     provider: tavily               # tavily | brave | gateway
#     tavily_api_key_env: TAVILY_API_KEY
#     top_k: 5
#     timeout_seconds: 10
#   fetch:
#     timeout_seconds: 30
#     cache_ttl_seconds: 600

# ── 并发 ────────────────────────────────────────────────────
# runtime:
#   max_concurrent_turns: 5

# ── 展示 / 成本 ─────────────────────────────────────────────
# currency: "$"
`
	os.WriteFile(userPath, []byte(template), 0o644)
}

// LoadConfigBytes parses configuration from raw YAML bytes (nil or empty =>
// built-in defaults), applying the same normalization and validation as
// LoadConfig. Embedded hosts (iOS/macOS in-app) supply config in-memory rather
// than from a file path, since the app sandbox has no fixed config.yaml.
func LoadConfigBytes(data []byte) (Config, error) {
	cfg := Config{
		Agent:  AgentConfig{MaxSteps: 8},
		Server: ServerConfig{PublicHealthz: true},
	}

	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, err
		}
	}

	// Backward compatibility applies only when the models field is absent. An
	// explicit models: {} is the host's zero-Provider read-only mode and must
	// remain empty.
	if cfg.Models == nil {
		cfg.Models = map[string]ModelConfig{
			"deepseek": {
				Provider: "openai",
				BaseURL:  "https://api.deepseek.com",
				Model:    "deepseek-v4-flash",
			},
		}
	}

	if len(cfg.Models) > 0 && cfg.DefaultModel == "" {
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
		// R1.5: flag the legacy api_key_env path BEFORE the registry fills it,
		// so only user-written api_key_env (from YAML) triggers it.
		if mc.APIKeyEnv != "" {
			cfg.Warnings = append(cfg.Warnings,
				fmt.Sprintf("model %q uses legacy api_key_env %q; migrate to the credentials section (api_key_env is deprecated)", name, mc.APIKeyEnv))
		}
		// R2.1: fill base_url / api_key_env from the built-in registry when the
		// config leaves them empty (registry never overrides explicit values).
		applyRegistryDefaults(&mc)
		if mc.Temperature <= 0 {
			mc.Temperature = 0.2
		}
		if mc.ContextWindow <= 0 {
			mc.ContextWindow = defaultContextWindow
		}

		// Normalise credential ref: if not explicitly set, derive from the
		// resolved state. api_key_env or a local base URL drive the derivation.
		if mc.Credential.IsZero() {
			if mc.APIKeyEnv != "" {
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
	// Explicit models: {} means the host intentionally wants no models. A
	// non-empty default_model contradicts that intent — catch it early.
	if len(cfg.Models) == 0 && cfg.DefaultModel != "" {
		return Config{}, fmt.Errorf("default_model %q cannot be set when models is empty", cfg.DefaultModel)
	}

	if cfg.Web.Search.Provider == "gateway" {
		if cfg.Web.Search.Credential.IsZero() {
			cfg.Web.Search.Credential = CredentialRef{Namespace: "gateway", Name: "default"}
		}
		// The common Gateway model/search deployment shares the same
		// /api/v1/agent base URL, so avoid requiring duplicate configuration.
		if cfg.Web.Search.GatewayBaseURL == "" {
			if mc, ok := cfg.Models[cfg.DefaultModel]; ok && mc.Credential == cfg.Web.Search.Credential {
				cfg.Web.Search.GatewayBaseURL = mc.BaseURL
			}
		}
		// Managed mode must never bypass Gateway billing via a local fallback.
		cfg.Web.Search.FallbackProvider = ""
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
		// R2.3: fall back to the built-in registry for known connection names so
		// `--model deepseek` / subagent_model: deepseek keep working even when
		// the config never declared them (the model/credential config is
		// user-level; the registry provides the open-box default). Unknown names
		// still error exactly as before.
		conn, known := builtinConnections[name]
		if !known {
			return ModelConfig{}, fmt.Errorf("unknown model %q; configured models: %s",
				name, strings.Join(c.ModelNames(), ", "))
		}
		mc = ModelConfig{
			Name:       name,
			Provider:   "openai",
			BaseURL:    conn.BaseURL,
			Model:      conn.WireModel,
			APIKeyEnv:  conn.Env,
			ContextWindow: defaultContextWindow,
			Temperature: 0.2,
		}
		if model.IsLocalBaseURL(mc.BaseURL) {
			mc.Credential = CredentialRef{} // none needed
		} else if conn.Env != "" {
			mc.Credential = CredentialRef{Namespace: "llm", Name: name}
		}
	}
	if mc.APIKeyEnv == "" && !model.IsLocalBaseURL(mc.BaseURL) && mc.Credential.IsZero() {
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
