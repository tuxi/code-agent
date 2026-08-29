// Package settings loads the project settings layer — Claude Code's settings.json
// model — that owns project-scoped BEHAVIOR (permissions now; verify and hooks in
// later phases), as opposed to config.yaml's INFRASTRUCTURE (models, API keys,
// endpoints). See docs/p11-project-settings.md.
//
// On disk the files are a subset of Claude Code's settings.json, layered in
// precedence order (low → high):
//
//	~/.codeagent/settings.json          — user, all your projects
//	<root>/.codeagent/settings.json     — project, shared (committable)
//	<root>/.codeagent/settings.local.json — project, this machine (git-ignored)
//
// Secrets never live here (the files are committable); API keys stay in
// config.yaml / env / the host keychain.
package settings

// TODO: optimize

import (
	"code-agent/internal/credential"
	"code-agent/internal/mcp"
	"code-agent/internal/session"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"code-agent/internal/hooks"
)

// Permissions is the tool-name allow/deny block, matched as globs downstream.
type Permissions struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
	// ProtectedPaths are file patterns (base names like ".env" or globs like
	// "*.key") that the safety layer treats as sensitive. Reading or writing them
	// is never auto-approved — even read-only tools trigger an audit event and
	// mutating tools require explicit confirmation regardless of allow rules.
	// Built-in defaults (see sandbox.DefaultProtectedPaths) always apply; this
	// list only ADDS paths, never removes them.
	ProtectedPaths []string `json:"protected_paths"`
}

// Verify is the finalize-verify block (P4.3-R Move 2, relocated here by P11.b).
// Command is a literal build/test command, "auto" to detect from the workspace
// (§8 of the design), or "" to disable. Enabled (nil = on when a command is set)
// can force it off while keeping the command documented.
type Verify struct {
	Command string `json:"command"`
	Enabled *bool  `json:"enabled"`
}

// File is the on-disk shape of one settings file — a subset of Claude Code's
// settings.json. Unknown keys are ignored on read (and preserved on write by the
// atomic writer that owns persistence), so a file authored for a newer version
// never breaks an older binary. P11.a models only permissions; verify and hooks
// blocks are added in P11.b/c.
//
// Since the config→settings merge (design-config-settings-merge.md §2.2), the
// file also carries the infrastructure blocks formerly in config.yaml:
// models/credentials/agent/provider/web/runtime/default_model/subagent_model.
// These are self-contained JSON structures (no dependency on internal/app), so
// the settings package stays a pure disk-format layer; app.Config is the
// runtime view built from them via FromSettings.
type File struct {
	// DefaultModel is the friendly model name used when none is selected.
	DefaultModel string `json:"default_model,omitempty"`
	// SubagentModel names the model a delegated read-only subagent runs on.
	SubagentModel string `json:"subagent_model,omitempty"`
	// Providers is the canonical way to declare models (design-providers-
	// grouped-config.md). A service id maps to its public config and models
	// list; per-model fields inherit from the service. This is the ONLY model
	// section — there is no flat "models" key.
	Providers map[string]ServiceConfig `json:"providers,omitempty"`
	// Credentials maps namespace → name → credential config (config.yaml
	// credentials:). Values are references (env var names / injected / none),
	// never secret values — settings.json is committable.
	Credentials map[string]map[string]CredentialConfig `json:"credentials,omitempty"`
	// Agent carries agent behavior knobs (max_steps, parallel tools, compaction).
	Agent AgentConfig `json:"agent,omitempty"`
	// Provider tunes the transport resilience layer (timeout/retry/backoff).
	Provider ProviderConfig `json:"provider,omitempty"`
	// Web configures the built-in web_search and web_fetch tools.
	Web WebConfig `json:"web,omitempty"`
	// Runtime controls process-wide turn admission.
	Runtime RuntimeConfig `json:"runtime,omitempty"`
	// Server carries the deployment-level server knobs (display_name, auth mode).
	// Secrets never live here: AccessToken is written as access_token_env (the env
	// var holding the value), never the token itself — settings.json is committable.
	Server ServerConfig `json:"server,omitempty"`
	// Currency is the display symbol for cost reporting.
	Currency string `json:"currency,omitempty"`

	Permissions Permissions `json:"permissions"`
	// Verify is a pointer so an ABSENT block (nil) is distinguishable from one set
	// to empty — the override merge needs that to know which layer "wins".
	Verify *Verify      `json:"verify"`
	Hooks  []hooks.Hook `json:"hooks"`
	// ApprovalMode is the workspace approval tier: "ask" (default), "auto", or
	// "full" (see internal/approve). It is a separate top-level key from
	// permissions.allow/deny — those stay owned by the approval card's "always
	// allow" grants; this key only selects the tier. Merges as an override, like
	// verify: the highest layer that sets it wins. Absent = "ask".
	ApprovalMode string `json:"approval_mode,omitempty"`
}

// ToSettings converts an on-disk settings document into the merged
// Settings view the runtime consumes. It mirrors the field list of the legacy
// explicit app.FromSettings call — infra blocks AND behavior blocks.
func (f File) ToSettings() Settings {
	return Settings{
		DefaultModel:  f.DefaultModel,
		SubagentModel: f.SubagentModel,
		Providers:     f.Providers,
		Credentials:   f.Credentials,
		Agent:         f.Agent,
		Provider:      f.Provider,
		Web:           f.Web,
		Runtime:       f.Runtime,
		Server:        f.Server,
		Currency:      f.Currency,
		Permissions:   f.Permissions,
		Verify:        f.Verify,
		Hooks:         f.Hooks,
		ApprovalMode:  f.ApprovalMode,
	}
}

// ModelConfig is the on-disk model definition (mirrors app.ModelConfig's
// user-configurable subset). Self-contained here so settings does not depend on
// internal/app.
type ModelConfig struct {
	Provider            string               `json:"provider,omitempty"`       // "openai" | "responses" | "ollama"
	BaseURL             string               `json:"base_url,omitempty"`       // API base URL
	Model               string               `json:"model,omitempty"`          // wire model string
	APIKeyEnv           string               `json:"api_key_env,omitempty"`    // legacy env var name
	Temperature         float64              `json:"temperature,omitempty"`    // default 0.2
	ContextWindow       int                  `json:"context_window,omitempty"` // compaction threshold
	InputPricePerM      float64              `json:"input_price_per_million,omitempty"`
	OutputPricePerM     float64              `json:"output_price_per_million,omitempty"`
	CacheInputPricePerM float64              `json:"cache_input_price_per_million,omitempty"`
	Credential          CredentialRef        `json:"credential,omitempty"`
	Catalog             ModelCatalogMetadata `json:"catalog,omitempty"`
	// WebSearch enables the provider's built-in web_search tool (Responses API).
	WebSearch bool `json:"web_search,omitempty"`

	CompactRatio float64 `json:"compact_ratio,omitempty"`

	// Resolved at load time, not read from YAML.
	Name string `json:"-"` // the friendly name (the map key)
}

// SupportsVision reports whether this model declares image input among its
// input modalities. It is the single source of truth for the runtime's decision
// to inject local image attachments as multimodal content parts; models
// without an explicit "image" modality stay text-only (fail-safe default).
func (mc ModelConfig) SupportsVision() bool {
	for _, m := range mc.Catalog.InputModalities {
		if m == "image" {
			return true
		}
	}
	return false
}

// ProviderName returns the display provider brand behind this model config —
// the service id (e.g. "deepseek", "openrouter", "qwen") when known, falling
// back to the friendly config key. It is distinct from Provider, which is the
// wire transport type ("openai" | "responses" | "ollama"), and from Model,
// which is the wire model string. Clients use it to show which provider
// actually served a conversation and to offer a model correction in the
// session detail.
func (mc ModelConfig) ProviderName() string {
	if mc.Catalog.ProviderID != "" {
		return mc.Catalog.ProviderID
	}
	return mc.Name
}

// ServiceConfig is one grouped service (design-providers-grouped-config.md
// §3.1). Fields at this level inherit to every model in Models. Headers hold
// ENV references only (e.g. "Bearer ${OPENROUTER_API_KEY}") — values never
// live here (settings.json is committable).
type ServiceConfig struct {
	// Enabled disables the service without deleting its config (OQ1): the
	// models are skipped at expansion, but the definition is preserved.
	// Defaults to true when absent.
	Enabled    *bool             `json:"enabled,omitempty"`
	BaseURL    string            `json:"base_url,omitempty"`   // API base URL (registry fills when omitted for known services)
	API        string            `json:"api,omitempty"`        // api type: "openai" | "responses" | "ollama" (registry derives for known)
	Credential CredentialRef     `json:"credential,omitempty"` // credential ref (defaults to llm/<id> when omitted)
	Headers    map[string]string `json:"headers,omitempty"`    // extra request headers; env refs only
	Models     []ProviderModel   `json:"models"`               // model list; each entry carries per-model differences only
}

// ProviderModel is one model within a grouped provider. ID is the wire model
// string sent to the API. All other fields are optional overrides that inherit
// from the provider when zero.
type ProviderModel struct {
	ID                  string   `json:"id"`
	RuntimeAlias        string   `json:"runtime_alias,omitempty"` // short friendly name usable as default_model
	API                 string   `json:"api,omitempty"`           // per-model override of the provider's api type ("openai" | "responses" | "ollama")
	ContextWindow       int      `json:"context_window,omitempty"`
	Temperature         float64  `json:"temperature,omitempty"`
	InputPricePerM      float64  `json:"input_price_per_million,omitempty"`
	OutputPricePerM     float64  `json:"output_price_per_million,omitempty"`
	CacheInputPricePerM float64  `json:"cache_input_price_per_million,omitempty"`
	SupportsTools       *bool    `json:"supports_tools,omitempty"`
	SupportsReasoning   *bool    `json:"supports_reasoning,omitempty"`
	InputModalities     []string `json:"input_modalities,omitempty"`
	WebSearch           bool     `json:"web_search,omitempty"`
	CompactRatio        float64  `json:"compact_ratio,omitempty"`
}

// CredentialRef points to a credential entry in Config.Credentials.
type CredentialRef struct {
	Namespace string `json:"namespace,omitempty"` // "gateway" | "llm" | "mcp"
	Name      string `json:"name,omitempty"`      // "default" | "deepseek" | "github"
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
	Source string `json:"source,omitempty"` // "env" | "injected" | "none"
	Env    string `json:"env,omitempty"`    // env var name (when source == "env")
}

type AgentConfig struct {
	MaxSteps int `json:"max_steps,omitempty"`

	// CompactRatio is the fraction of a model's context window at which the
	// session compacts. Defaults to defaultCompactRatio; values outside (0,1) are
	// treated as unset.
	CompactRatio float64 `json:"compact_ratio,omitempty"`

	// CompactKeepRatio is the fraction of the compaction threshold kept as the
	// verbatim recent tail when compacting (P12.a). Token-denominated: the tail
	// is CompactThreshold × CompactKeepRatio approximate tokens, which is what
	// makes compaction converge by construction — summary + bounded tail lands
	// back under the threshold on a 32k local window as much as on a 128k one.
	// Defaults to defaultCompactKeepRatio; values outside (0,1) are treated as
	// unset.
	CompactKeepRatio float64 `json:"compact_keep_ratio,omitempty"`

	// SubagentModel names the model a delegated read-only subagent (the `task`
	// tool, 8.3) runs on. Empty inherits the main model; point it at a cheaper
	// model (e.g. a flash-class one) to make read-only investigation cheap. An
	// unknown or key-less name falls back to the main model at runtime.
	SubagentModel string `json:"subagent_model,omitempty"`

	// ClientToolTimeoutSeconds is the lease for a single client-executed tool
	// call (v1.1): how long the loop blocks waiting for the client to deliver a
	// tool_result before giving up with "client timeout". 0 uses the built-in
	// 2-minute default. Raise it when clients run long operations — e.g. a
	// DreamAI sidecar whose generate tool drives image/video generation that
	// routinely exceeds two minutes.
	ClientToolTimeoutSeconds int `json:"client_tool_timeout_seconds,omitempty"`

	// MaxParallelTools caps how many independent, read-only tool calls in one
	// batch execute concurrently (P8.8). 0/1 keeps the strictly sequential loop
	// (the default). Raising it lets the model fan out — e.g. 5 `task` subagents
	// in one turn run at once. Side-effecting calls are always serialized.
	MaxParallelTools int `json:"max_parallel_tools,omitempty"`

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
	BuiltinTools *[]string `json:"builtin_tools,omitempty"`
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

// ProviderConfig tunes the transport resilience layer.
type ProviderConfig struct {
	RequestTimeoutSeconds int `json:"request_timeout_seconds,omitempty"`
	MaxRetries            int `json:"max_retries,omitempty"`
	BackoffMillis         int `json:"backoff_millis,omitempty"`
	MaxBackoffSeconds     int `json:"max_backoff_seconds,omitempty"`
}

// WebConfig configures web_search and web_fetch.
type WebConfig struct {
	Search WebSearchConfig `json:"search,omitempty"`
	Fetch  WebFetchConfig  `json:"fetch,omitempty"`
}

// WebSearchConfig configures the web_search tool.
type WebSearchConfig struct {
	Provider              string        `json:"provider,omitempty"`
	FallbackProvider      string        `json:"fallback_provider,omitempty"`
	GatewayBaseURL        string        `json:"gateway_base_url,omitempty"`
	TopK                  int           `json:"top_k,omitempty"`
	TimeoutSeconds        int           `json:"timeout_seconds,omitempty"`
	TavilyAPIKeyEnv       string        `json:"tavily_api_key_env,omitempty"`
	BraveAPIKeyEnv        string        `json:"brave_api_key_env,omitempty"`
	SearXNGBaseURL        string        `json:"searxng_base_url,omitempty"`
	GatewayTimeoutSeconds int           `json:"gateway_timeout_seconds,omitempty"`
	Credential            CredentialRef `json:"credential,omitempty"`

	// Resolved at load time or injected by a host (e.g. iOS Keychain), not read
	// from YAML. Same pattern as ModelConfig.APIKey.
	BraveKey  string `json:"-"`
	TavilyKey string `json:"-"`
}

// WebFetchConfig configures the web_fetch tool.
type WebFetchConfig struct {
	TimeoutSeconds  int `json:"timeout_seconds,omitempty"`
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
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

// RuntimeConfig controls process-wide turn admission.
type RuntimeConfig struct {
	MaxConcurrentTurns int `json:"max_concurrent_turns,omitempty"`
}

// ServerConfig carries deployment-level server knobs (config.yaml server:).
// This is a user-level (deployment) concern — the project-scope file should not
// carry it. AccessTokenEnv names the env var holding the token (preferred for
// committable files); AccessToken holds the value directly when the user
// chooses to store it in the user-level file.
type ServerConfig struct {
	DisplayName    string `json:"display_name,omitempty"`
	Authentication string `json:"authentication,omitempty"` // none | bearer
	AccessTokenEnv string `json:"access_token_env,omitempty"`
	AccessToken    string `json:"access_token,omitempty"`
	PublicHealthz  bool   `json:"public_healthz,omitempty"`
	TLSCertificate string `json:"tls_certificate,omitempty"`
	TLSPrivateKey  string `json:"tls_private_key,omitempty"`
}

// ModelCatalogMetadata is optional non-secret presentation metadata.
type ModelCatalogMetadata struct {
	ConnectionID          string   `json:"connection_id,omitempty"`
	ProviderID            string   `json:"provider_id,omitempty"`
	ConnectionDisplayName string   `json:"connection_display_name,omitempty"`
	DisplayName           string   `json:"display_name,omitempty"`
	SupportsTools         *bool    `json:"supports_tools,omitempty"`
	SupportsReasoning     bool     `json:"supports_reasoning,omitempty"`
	InputModalities       []string `json:"input_modalities,omitempty"`
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

// Settings is the merged view across the layers. Permissions merge as a UNION
// (deny wins downstream); Verify merges as an OVERRIDE (the highest layer that
// sets a block wins); Hooks CONCATENATE (every layer's hooks all run, in layer
// order). Infrastructure blocks (models/credentials/agent/provider/web/runtime)
// merge per-field: the highest layer that sets a field wins, else the lower
// layer's value is inherited (config.yaml user → project semantics preserved).
type Settings struct {
	DefaultModel  string
	SubagentModel string
	Providers     map[string]ServiceConfig
	Credentials   map[string]map[string]CredentialConfig
	Agent         AgentConfig
	Provider      ProviderConfig
	Web           WebConfig
	Runtime       RuntimeConfig
	Server        ServerConfig
	Currency      string

	Permissions Permissions
	Verify      *Verify      // highest-priority layer's block, nil if no layer set one
	Hooks       []hooks.Hook // all layers' hooks, user → shared → local order

	// ApprovalMode is the effective approval tier ("ask"/"auto"/"full"); the
	// highest layer that set one wins, empty = "ask".
	ApprovalMode string

	// Bootstrapped is true when the user-scope settings file did not exist and
	// this load created the template. The TUI surfaces a first-run hint so a
	// new user knows where to configure models and credentials.
	Bootstrapped bool

	// GlobalSkillsDir is an optional directory of user-level skills loaded for every
	// workspace. Skills here act as a shared capability pool (always available); a
	// project-local skill of the same name takes precedence. Embedded hosts set it in
	// StartServer from the dataDir parameter. Code-level only (yaml:"-").
	GlobalSkillsDir string `json:"-"`

	// GlobalPromptsDir is an optional directory of user-level prompt templates.
	// Same pattern as GlobalSkillsDir: embedded hosts set it from dataDir; desktop
	// defaults to ~/.codeagent/prompts/.
	GlobalPromptsDir string `json:"-"`

	// Profile selects the platform capability set the runtime assembles for. It is
	// code-level only (set by the embedded host, not the YAML) so a desktop config
	// file can never accidentally downgrade itself. Default (full) assumes a host
	// that can spawn subprocesses and reach the whole filesystem; Sandboxed is for
	// embedded hosts like iOS. See Profile.
	Profile Profile `json:"-"`

	// MCP configures external Model Context Protocol servers whose tools are
	// registered alongside the built-in ones. It is code-level only (yaml:"-"):
	// MCP servers are configured in a separate Claude-compatible `.mcp.json`
	// document, not in this YAML, so a config authored for Claude Code is consumed
	// verbatim. CLI entry points (run/repl/tui) populate this from CWD's
	// `.mcp.json` via mcp.ResolveDesktop; the daemon (codeagentd) leaves it empty
	// and resolves MCP per conversation workspace via WorkspaceRegistry.
	// Embedded hosts (iOS/macOS) inject it in-memory (see embed.Options.MCPJSON).
	// Empty (the default) disables it.
	MCP mcp.Config `json:"-"`

	Models map[string]ModelConfig `json:"-"`

	// Warnings collects non-fatal configuration notes raised during load (e.g.
	// legacy api_key_env usage slated for removal). Printed by CLI entry points
	// at startup; empty when the config is clean. Code-level only (yaml:"-").
	Warnings []string `json:"-"`

	// StoreFactory, if set, creates the session store for a workspace root.
	// When nil (default), the built-in SQLite store is used (backward compatible).
	// External consumers that want their own storage backend (e.g. PostgreSQL)
	// set this to their own factory. The returned Store owns its lifecycle;
	// callers must Close it. This field is code-level only (yaml:"-").
	StoreFactory session.StoreFactory `json:"-"`
}

// UserPath is the user-scope file, shared across all your projects. Empty home
// (unresolvable) disables it.
func UserPath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codeagent", "settings.json")
}

// ProjectSharedPath is the committable, team-shared project file. Empty root
// disables it.
func ProjectSharedPath(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".codeagent", "settings.json")
}

// ProjectLocalPath is the git-ignored, machine-local project file — the default
// target for an interactive grant or an agent-written setting. Empty root
// disables it.
func ProjectLocalPath(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".codeagent", "settings.local.json")
}

// LoadFile reads and parses one settings file. A missing file is not an error
// (absence is normal) — it returns the zero File. A present-but-malformed file
// returns an error so the caller can log-and-skip rather than fail startup.
func LoadFile(path string) (File, error) {
	var f File
	if path == "" {
		return f, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}

// Load reads the layer files for root/home in precedence order and returns the
// merged Settings. Best-effort: a missing file is skipped, and a malformed one is
// logged to warn (nil = os.Stderr) and skipped, so a corrupt machine-written file
// never bricks startup. Empty root disables the project files; empty home
// disables the user file.
func Load(root, home string, warn io.Writer) Settings {
	if warn == nil {
		warn = os.Stderr
	}
	var s Settings
	// First start: write a minimal user settings.json template so the user knows
	// the file exists and where to configure models/credentials. Best-effort.
	if up := UserPath(home); up != "" {
		if _, err := os.Stat(up); err != nil && errors.Is(err, os.ErrNotExist) {
			s.Bootstrapped = bootstrapUserSettings(up)
		}
	}
	for _, path := range []string{UserPath(home), ProjectSharedPath(root), ProjectLocalPath(root)} {
		mergeFileIntoSettings(&s, path, warn)
	}
	return s
}

// LoadUserOnly loads only the user-scope settings file. Used before trust
// resolution so project_trust hooks (defined in user settings) are available
// before project files are loaded.
func LoadUserOnly(home string, warn io.Writer) Settings {
	if warn == nil {
		warn = os.Stderr
	}
	var s Settings
	up := UserPath(home)
	if up != "" {
		if _, err := os.Stat(up); err != nil && errors.Is(err, os.ErrNotExist) {
			s.Bootstrapped = bootstrapUserSettings(up)
		}
	}
	mergeFileIntoSettings(&s, up, warn)
	return s
}

// LoadProjectSettings loads only the project-scoped settings files (shared +
// local). Called conditionally after trust resolution passes.
func LoadProjectSettings(root string, warn io.Writer) Settings {
	if warn == nil {
		warn = os.Stderr
	}
	var s Settings
	mergeFileIntoSettings(&s, ProjectSharedPath(root), warn)
	mergeFileIntoSettings(&s, ProjectLocalPath(root), warn)
	return s
}

// MergeSettings merges overlay settings into base using the same per-field
// rules as Load (permissions union, hooks concatenate, verify override,
// infrastructure per-field). Used to combine staged loads.
func MergeSettings(base *Settings, overlay Settings) {
	base.Permissions.Allow = appendUnique(base.Permissions.Allow, overlay.Permissions.Allow...)
	base.Permissions.Deny = appendUnique(base.Permissions.Deny, overlay.Permissions.Deny...)
	base.Permissions.ProtectedPaths = appendUnique(base.Permissions.ProtectedPaths, overlay.Permissions.ProtectedPaths...)
	if overlay.Verify != nil {
		base.Verify = overlay.Verify
	}
	// ApprovalMode is an override scalar like verify: the highest layer that
	// sets it wins (local > shared > user).
	if overlay.ApprovalMode != "" {
		base.ApprovalMode = overlay.ApprovalMode
	}
	base.Hooks = append(base.Hooks, overlay.Hooks...)
	if overlay.DefaultModel != "" {
		base.DefaultModel = overlay.DefaultModel
	}
	if overlay.SubagentModel != "" {
		base.SubagentModel = overlay.SubagentModel
	}
	if base.Providers == nil {
		base.Providers = map[string]ServiceConfig{}
	}
	for id, pc := range overlay.Providers {
		base.Providers[id] = pc
	}
	if base.Credentials == nil {
		base.Credentials = map[string]map[string]CredentialConfig{}
	}
	for ns, entries := range overlay.Credentials {
		if base.Credentials[ns] == nil {
			base.Credentials[ns] = map[string]CredentialConfig{}
		}
		for name, cc := range entries {
			base.Credentials[ns][name] = cc
		}
	}
	if overlay.Agent.MaxSteps != 0 {
		base.Agent.MaxSteps = overlay.Agent.MaxSteps
	}
	if overlay.Agent.MaxParallelTools != 0 {
		base.Agent.MaxParallelTools = overlay.Agent.MaxParallelTools
	}
	if overlay.Agent.CompactRatio > 0 && overlay.Agent.CompactRatio < 1 {
		base.Agent.CompactRatio = overlay.Agent.CompactRatio
	}
	if overlay.Agent.CompactKeepRatio > 0 && overlay.Agent.CompactKeepRatio < 1 {
		base.Agent.CompactKeepRatio = overlay.Agent.CompactKeepRatio
	}
	if overlay.Agent.ClientToolTimeoutSeconds != 0 {
		base.Agent.ClientToolTimeoutSeconds = overlay.Agent.ClientToolTimeoutSeconds
	}
	if overlay.Agent.SubagentModel != "" {
		base.Agent.SubagentModel = overlay.Agent.SubagentModel
	}
	if overlay.Provider.RequestTimeoutSeconds != 0 {
		base.Provider.RequestTimeoutSeconds = overlay.Provider.RequestTimeoutSeconds
	}
	if overlay.Provider.MaxRetries != 0 {
		base.Provider.MaxRetries = overlay.Provider.MaxRetries
	}
	if overlay.Provider.BackoffMillis != 0 {
		base.Provider.BackoffMillis = overlay.Provider.BackoffMillis
	}
	if overlay.Provider.MaxBackoffSeconds != 0 {
		base.Provider.MaxBackoffSeconds = overlay.Provider.MaxBackoffSeconds
	}
	if overlay.Web.Search.Provider != "" {
		base.Web.Search.Provider = overlay.Web.Search.Provider
	}
	if overlay.Web.Search.FallbackProvider != "" {
		base.Web.Search.FallbackProvider = overlay.Web.Search.FallbackProvider
	}
	if overlay.Web.Search.GatewayBaseURL != "" {
		base.Web.Search.GatewayBaseURL = overlay.Web.Search.GatewayBaseURL
	}
	if overlay.Web.Search.TopK != 0 {
		base.Web.Search.TopK = overlay.Web.Search.TopK
	}
	if overlay.Web.Search.TimeoutSeconds != 0 {
		base.Web.Search.TimeoutSeconds = overlay.Web.Search.TimeoutSeconds
	}
	if overlay.Web.Search.TavilyAPIKeyEnv != "" {
		base.Web.Search.TavilyAPIKeyEnv = overlay.Web.Search.TavilyAPIKeyEnv
	}
	if overlay.Web.Search.BraveAPIKeyEnv != "" {
		base.Web.Search.BraveAPIKeyEnv = overlay.Web.Search.BraveAPIKeyEnv
	}
	if overlay.Web.Fetch.TimeoutSeconds != 0 {
		base.Web.Fetch.TimeoutSeconds = overlay.Web.Fetch.TimeoutSeconds
	}
	if overlay.Web.Fetch.CacheTTLSeconds != 0 {
		base.Web.Fetch.CacheTTLSeconds = overlay.Web.Fetch.CacheTTLSeconds
	}
	if overlay.Runtime.MaxConcurrentTurns != 0 {
		base.Runtime.MaxConcurrentTurns = overlay.Runtime.MaxConcurrentTurns
	}
	if overlay.Server.DisplayName != "" && overlay.Server.Authentication != "" {
		base.Server = overlay.Server
	}
	if overlay.Currency != "" {
		base.Currency = overlay.Currency
	}
}

// mergeFileIntoSettings loads one settings file and merges it into s using the
// per-field rules. Missing or malformed files are logged and skipped.
func mergeFileIntoSettings(s *Settings, path string, warn io.Writer) {
	if path == "" {
		return
	}
	f, err := LoadFile(path)
	if err != nil {
		fmt.Fprintf(warn, "[settings] ignoring malformed %s: %v\n", path, err)
		return
	}
	MergeSettings(s, Settings{
		Permissions:   f.Permissions,
		Verify:        f.Verify,
		Hooks:         f.Hooks,
		ApprovalMode:  f.ApprovalMode,
		Providers:     f.Providers,
		Credentials:   f.Credentials,
		Agent:         f.Agent,
		Provider:      f.Provider,
		Web:           f.Web,
		Runtime:       f.Runtime,
		Server:        f.Server,
		DefaultModel:  f.DefaultModel,
		SubagentModel: f.SubagentModel,
		Currency:      f.Currency,
	})
}

// bootstrapUserSettings writes a minimal-but-valid user settings.json template
// on first start (design-config-settings-merge.md stage C). The file is legal
// JSON — the configurable sections are present with empty values so the user
// fills them in (the registry already provides open-box models, so an empty
// settings.json is fully functional). Best-effort: failure is silent, the
// runtime stays usable via built-in defaults. The file is never overwritten
// once it exists (the caller checks os.ErrNotExist first). Reports whether the
// template was actually created (true = first run).
func bootstrapUserSettings(path string) bool {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	const template = `{
  "default_model": "deepseek/deepseek-v4-flash",
  "credentials": {
    "llm": {
      "deepseek": {
        "source": "env",
        "env": "DEEPSEEK_API_KEY"
      }
    }
  },
  "agent": {
    "client_tool_timeout_seconds": 900,
    "compact_ratio": 0.75,
    "max_parallel_tools": 10,
    "max_steps": 120
  },
  "provider": {
    "request_timeout_seconds": 0,
    "max_retries": 0,
    "backoff_millis": 0,
    "max_backoff_seconds": 0
  },
  "providers": {
    "deepseek": {
      "enabled": true,
      "base_url": "https://api.deepseek.com",
      "api": "openai",
      "credential": {
        "namespace": "llm",
        "name": "deepseek"
      },
      "models": [
        {
          "id": "deepseek-v4-flash",
          "context_window": 1000000,
          "runtime_alias": "deepseek v4 flash",
          "temperature": 0.2,
          "input_price_per_million": 0.16,
          "output_price_per_million": 0.32,
          "cache_input_price_per_million": 0.003,
          "web_search": true,
          "supports_tools": true,
          "input_modalities": [
            "text"
          ]
        },
        {
          "id": "deepseek-v4-pro",
          "context_window": 1000000,
          "runtime_alias": "deepseek v4 pro",
          "temperature": 0.2,
          "input_price_per_million": 0.45,
          "output_price_per_million": 0.9,
          "cache_input_price_per_million": 0.0038,
          "supports_tools": true,
          "input_modalities": [
            "text"
          ]
        }
      ]
    }
  },
  "web": {
    "search": {
      "provider": "",
      "tavily_api_key_env": ""
    },
    "fetch": {
      "timeout_seconds": 0,
      "cache_ttl_seconds": 0
    }
  },
  "runtime": {
    "max_concurrent_turns": 0
  },
  "server": {
    "display_name": "",
    "authentication": "",
    "access_token_env": "",
    "public_healthz": false
  },
  "currency": "$"
}
`
	if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
		return false
	}
	return true
}

// ResolveVerify returns the effective finalize-verify command for the workspace:
// the settings layer's verify block if any layer set one, else the config.yaml
// legacy value (P4.3-R Move 2's agent.verify_command, layer 0). "" means
// auto-detect from workspace files (go.mod → go build, Cargo.toml → cargo build,
// etc.). Explicit "off"/"false"/"none"/"disabled" disables verification.
func ResolveVerify(root, home, legacy string) string {
	return ResolveVerifyFrom(Load(root, home, os.Stderr), root, legacy)
}

// ResolveVerifyFrom is ResolveVerify against an already-loaded Settings, so a
// caller that also needs other blocks (e.g. hooks) loads the layer only once.
func ResolveVerifyFrom(s Settings, root, legacy string) string {
	if s.Verify != nil {
		if s.Verify.Enabled != nil && !*s.Verify.Enabled {
			return ""
		}
		return resolveVerifyCommand(s.Verify.Command, root)
	}
	return resolveVerifyCommand(legacy, root)
}

// resolveVerifyCommand maps a raw command string to the command to run: the
// disable words and "" → off; "auto" → detected; anything else verbatim.
func resolveVerifyCommand(cmd, root string) string {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "off", "false", "none", "disabled":
		return "" // explicit opt-out
	case "auto", "":
		// Default to auto-detection when not configured — same behaviour as
		// Claude Code. Falls through to DetectVerify which returns "" when
		// nothing is recognised, so a Python-only repo is still silent.
		return DetectVerify(root)
	default:
		return cmd
	}
}

// DetectVerify infers a cheap, side-effect-free build command from the files at
// the workspace root (§8). First match wins; build-class over full test suites so
// auto-running at every finish stays cheap. "" when nothing recognizable is found.
func DetectVerify(root string) string {
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, rel))
		return err == nil
	}
	switch {
	case exists("go.mod"):
		return "go build ./..."
	case exists("Cargo.toml"):
		return "cargo build"
	case exists("package.json"):
		if cmd := detectNodeVerify(filepath.Join(root, "package.json")); cmd != "" {
			return cmd
		}
	}
	if exists("Package.swift") {
		return "swift build"
	}
	if xs, _ := filepath.Glob(filepath.Join(root, "*.xcodeproj")); len(xs) > 0 {
		return "swift build"
	}
	return ""
}

// detectNodeVerify picks a build/test npm script from a package.json, preferring
// the cheaper "build". "" when neither script exists (so detection falls through).
func detectNodeVerify(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	if _, ok := pkg.Scripts["build"]; ok {
		return "npm run build"
	}
	if _, ok := pkg.Scripts["test"]; ok {
		return "npm test"
	}
	return ""
}

// appendUnique appends the non-empty, not-yet-present items of xs to dst.
func appendUnique(dst []string, xs ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(xs))
	for _, d := range dst {
		seen[d] = struct{}{}
	}
	for _, x := range xs {
		if x == "" {
			continue
		}
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		dst = append(dst, x)
	}
	return dst
}

// CompactThreshold is the prompt-token count at which a session running the
// given model should compact: the model's context window scaled by the
// configured compact ratio. This is what makes compaction model-aware — a
// 256k-window model gets a proportionally higher threshold than a 128k one.
func (c Settings) CompactThreshold(mc ModelConfig) int {
	return int(float64(mc.ContextWindow) * c.Agent.CompactRatio)
}

// CompactKeepTokens is the approximate token budget for the verbatim recent
// tail kept by compaction (P12.a): the compaction threshold scaled by the keep
// ratio. Everything older is folded into the summary.
func (c Settings) CompactKeepTokens(mc ModelConfig) int {
	return int(float64(c.CompactThreshold(mc)) * c.Agent.CompactKeepRatio)
}

// CredentialResolver builds a credential.Resolver from the configured
// credentials section and environment. It returns a ChainResolver that tries
// (in order): injected credentials (from the secrets map, populated by the
// caller after LoadConfigBytes), environment variables, and explicit "none".
//
// When the credentials section is empty and no external resolver has been
// injected, a plain EnvResolver is returned (CLI backward compat).
func (c Settings) CredentialResolver(injected credential.Resolver) credential.Resolver {
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
func (c Settings) ModelNames() []string {
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

func (c Settings) RuntimeMaxConcurrentTurns() int {
	if c.Runtime.MaxConcurrentTurns < 1 {
		return 1
	}
	return c.Runtime.MaxConcurrentTurns
}

// DisplayModelName returns a human-readable label for a model key. A grouped-
// provider model (alias key provider.<b64>.model.<b64>) becomes
// "<provider-id>/<model-id>"; any other key is returned unchanged. The TUI
// /use picker and REPL /models use this so users never see base64 alias keys.
func (c Settings) DisplayModelName(key string) string {
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
