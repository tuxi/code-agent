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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
}

// CredentialRef references a credentials entry (namespace/name).
type CredentialRef struct {
	Namespace string `json:"namespace,omitempty"` // "gateway" | "llm" | "mcp"
	Name      string `json:"name,omitempty"`
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
}

// CredentialConfig describes how a named credential is obtained.
type CredentialConfig struct {
	Source string `json:"source,omitempty"` // "env" | "injected" | "none"
	Env    string `json:"env,omitempty"`    // env var name (source == env)
}

// AgentConfig carries agent behavior knobs.
type AgentConfig struct {
	MaxSteps                 int     `json:"max_steps,omitempty"`
	MaxParallelTools         int     `json:"max_parallel_tools,omitempty"`
	CompactRatio             float64 `json:"compact_ratio,omitempty"`
	CompactKeepRatio         float64 `json:"compact_keep_ratio,omitempty"`
	ClientToolTimeoutSeconds int     `json:"client_tool_timeout_seconds,omitempty"`
	SubagentModel            string  `json:"subagent_model,omitempty"`
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
	Provider         string `json:"provider,omitempty"`
	FallbackProvider string `json:"fallback_provider,omitempty"`
	GatewayBaseURL   string `json:"gateway_base_url,omitempty"`
	TopK             int    `json:"top_k,omitempty"`
	TimeoutSeconds   int    `json:"timeout_seconds,omitempty"`
	TavilyAPIKeyEnv  string `json:"tavily_api_key_env,omitempty"`
	BraveAPIKeyEnv   string `json:"brave_api_key_env,omitempty"`
}

// WebFetchConfig configures the web_fetch tool.
type WebFetchConfig struct {
	TimeoutSeconds  int `json:"timeout_seconds,omitempty"`
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
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
	// First start: write a minimal user settings.json template so the user knows
	// the file exists and where to configure models/credentials. Best-effort.
	if up := UserPath(home); up != "" {
		if _, err := os.Stat(up); errors.Is(err, os.ErrNotExist) {
			bootstrapUserSettings(up)
		}
	}
	var s Settings
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
	up := UserPath(home)
	if up != "" {
		if _, err := os.Stat(up); errors.Is(err, os.ErrNotExist) {
			bootstrapUserSettings(up)
		}
	}
	var s Settings
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
// once it exists (the caller checks os.ErrNotExist first).
func bootstrapUserSettings(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
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
	os.WriteFile(path, []byte(template), 0o644)
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
