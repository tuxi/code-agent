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
	// Models is the friendly-name → model config map (config.yaml models:).
	Models map[string]ModelConfig `json:"models,omitempty"`
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
	Provider     string         `json:"provider,omitempty"`     // "openai" | "ollama"
	BaseURL      string         `json:"base_url,omitempty"`     // API base URL
	Model        string         `json:"model,omitempty"`        // wire model string
	APIKeyEnv    string         `json:"api_key_env,omitempty"`  // legacy env var name
	Temperature  float64        `json:"temperature,omitempty"`  // default 0.2
	ContextWindow int           `json:"context_window,omitempty"` // compaction threshold
	InputPricePerM  float64     `json:"input_price_per_million,omitempty"`
	OutputPricePerM float64     `json:"output_price_per_million,omitempty"`
	CacheInputPricePerM float64 `json:"cache_input_price_per_million,omitempty"`
	Credential   CredentialRef  `json:"credential,omitempty"`
	Catalog      ModelCatalogMetadata `json:"catalog,omitempty"`
}

// CredentialRef references a credentials entry (namespace/name).
type CredentialRef struct {
	Namespace string `json:"namespace,omitempty"` // "gateway" | "llm" | "mcp"
	Name      string `json:"name,omitempty"`
}

// CredentialConfig describes how a named credential is obtained.
type CredentialConfig struct {
	Source string `json:"source,omitempty"` // "env" | "injected" | "none"
	Env    string `json:"env,omitempty"`    // env var name (source == env)
}

// AgentConfig carries agent behavior knobs.
type AgentConfig struct {
	MaxSteps                int     `json:"max_steps,omitempty"`
	MaxParallelTools        int     `json:"max_parallel_tools,omitempty"`
	CompactRatio            float64 `json:"compact_ratio,omitempty"`
	CompactKeepRatio        float64 `json:"compact_keep_ratio,omitempty"`
	ClientToolTimeoutSeconds int    `json:"client_tool_timeout_seconds,omitempty"`
	SubagentModel           string  `json:"subagent_model,omitempty"`
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
	Provider        string `json:"provider,omitempty"`
	FallbackProvider string `json:"fallback_provider,omitempty"`
	GatewayBaseURL  string `json:"gateway_base_url,omitempty"`
	TopK            int    `json:"top_k,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
	TavilyAPIKeyEnv string `json:"tavily_api_key_env,omitempty"`
	BraveAPIKeyEnv  string `json:"brave_api_key_env,omitempty"`
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
	Models        map[string]ModelConfig
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
		if path == "" {
			continue
		}
		f, err := LoadFile(path)
		if err != nil {
			fmt.Fprintf(warn, "[settings] ignoring malformed %s: %v\n", path, err)
			continue
		}
		s.Permissions.Allow = appendUnique(s.Permissions.Allow, f.Permissions.Allow...)
		s.Permissions.Deny = appendUnique(s.Permissions.Deny, f.Permissions.Deny...)
		s.Permissions.ProtectedPaths = appendUnique(s.Permissions.ProtectedPaths, f.Permissions.ProtectedPaths...)
		// Verify overrides: iterating low → high, the last layer to set a block wins.
		if f.Verify != nil {
			s.Verify = f.Verify
		}
		// Hooks concatenate: every layer's hooks run, in layer order.
		s.Hooks = append(s.Hooks, f.Hooks...)

		// Infrastructure blocks merge per-field: iterating low → high, the last
		// layer to set a field wins (else the lower layer's value is inherited).
		// This preserves the config.yaml user → project semantics.
		if f.DefaultModel != "" {
			s.DefaultModel = f.DefaultModel
		}
		if f.SubagentModel != "" {
			s.SubagentModel = f.SubagentModel
		}
		if s.Models == nil {
			s.Models = map[string]ModelConfig{}
		}
		for name, mc := range f.Models {
			s.Models[name] = mc
		}
		if s.Credentials == nil {
			s.Credentials = map[string]map[string]CredentialConfig{}
		}
		for ns, entries := range f.Credentials {
			if s.Credentials[ns] == nil {
				s.Credentials[ns] = map[string]CredentialConfig{}
			}
			for name, cc := range entries {
				s.Credentials[ns][name] = cc
			}
		}
		if f.Agent.MaxSteps != 0 {
			s.Agent.MaxSteps = f.Agent.MaxSteps
		}
		if f.Agent.MaxParallelTools != 0 {
			s.Agent.MaxParallelTools = f.Agent.MaxParallelTools
		}
		if f.Agent.CompactRatio > 0 && f.Agent.CompactRatio < 1 {
			s.Agent.CompactRatio = f.Agent.CompactRatio
		}
		if f.Agent.CompactKeepRatio > 0 && f.Agent.CompactKeepRatio < 1 {
			s.Agent.CompactKeepRatio = f.Agent.CompactKeepRatio
		}
		if f.Agent.ClientToolTimeoutSeconds != 0 {
			s.Agent.ClientToolTimeoutSeconds = f.Agent.ClientToolTimeoutSeconds
		}
		if f.Agent.SubagentModel != "" {
			s.Agent.SubagentModel = f.Agent.SubagentModel
		}
		if f.Provider.RequestTimeoutSeconds != 0 {
			s.Provider.RequestTimeoutSeconds = f.Provider.RequestTimeoutSeconds
		}
		if f.Provider.MaxRetries != 0 {
			s.Provider.MaxRetries = f.Provider.MaxRetries
		}
		if f.Provider.BackoffMillis != 0 {
			s.Provider.BackoffMillis = f.Provider.BackoffMillis
		}
		if f.Provider.MaxBackoffSeconds != 0 {
			s.Provider.MaxBackoffSeconds = f.Provider.MaxBackoffSeconds
		}
		if f.Web.Search.Provider != "" {
			s.Web.Search.Provider = f.Web.Search.Provider
		}
		if f.Web.Search.FallbackProvider != "" {
			s.Web.Search.FallbackProvider = f.Web.Search.FallbackProvider
		}
		if f.Web.Search.GatewayBaseURL != "" {
			s.Web.Search.GatewayBaseURL = f.Web.Search.GatewayBaseURL
		}
		if f.Web.Search.TopK != 0 {
			s.Web.Search.TopK = f.Web.Search.TopK
		}
		if f.Web.Search.TimeoutSeconds != 0 {
			s.Web.Search.TimeoutSeconds = f.Web.Search.TimeoutSeconds
		}
		if f.Web.Search.TavilyAPIKeyEnv != "" {
			s.Web.Search.TavilyAPIKeyEnv = f.Web.Search.TavilyAPIKeyEnv
		}
		if f.Web.Search.BraveAPIKeyEnv != "" {
			s.Web.Search.BraveAPIKeyEnv = f.Web.Search.BraveAPIKeyEnv
		}
		if f.Web.Fetch.TimeoutSeconds != 0 {
			s.Web.Fetch.TimeoutSeconds = f.Web.Fetch.TimeoutSeconds
		}
		if f.Web.Fetch.CacheTTLSeconds != 0 {
			s.Web.Fetch.CacheTTLSeconds = f.Web.Fetch.CacheTTLSeconds
		}
		if f.Runtime.MaxConcurrentTurns != 0 {
			s.Runtime.MaxConcurrentTurns = f.Runtime.MaxConcurrentTurns
		}
		if f.Server.DisplayName != "" {
			s.Server.DisplayName = f.Server.DisplayName
		}
		if f.Server.Authentication != "" {
			s.Server.Authentication = f.Server.Authentication
		}
		if f.Server.AccessTokenEnv != "" {
			s.Server.AccessTokenEnv = f.Server.AccessTokenEnv
		}
		if f.Server.AccessToken != "" {
			s.Server.AccessToken = f.Server.AccessToken
		}
		if f.Server.PublicHealthz {
			s.Server.PublicHealthz = true
		}
		if f.Server.TLSCertificate != "" {
			s.Server.TLSCertificate = f.Server.TLSCertificate
		}
		if f.Server.TLSPrivateKey != "" {
			s.Server.TLSPrivateKey = f.Server.TLSPrivateKey
		}
		if f.Currency != "" {
			s.Currency = f.Currency
		}
	}
	return s
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
  "default_model": "",
  "subagent_model": "",
  "models": {},
  "credentials": {},
  "agent": {
    "max_steps": 0,
    "max_parallel_tools": 0,
    "compact_ratio": 0,
    "compact_keep_ratio": 0,
    "client_tool_timeout_seconds": 0,
    "subagent_model": ""
  },
  "provider": {
    "request_timeout_seconds": 0,
    "max_retries": 0,
    "backoff_millis": 0,
    "max_backoff_seconds": 0
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
