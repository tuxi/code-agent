package app

import (
	"code-agent/internal/model"
	"code-agent/internal/settings"
	"encoding/json"
	"fmt"
	"os"

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

// FromSettings builds a Config from the merged settings view
// (design-config-settings-merge.md). Config remains the runtime view — it
// carries code-level fields (MCP, Profile, StoreFactory, …) that settings.json
// never stores; FromSettings fills the user-configurable infrastructure subset
// and leaves the rest zero for the caller to assemble.
func FromSettings(set settings.Settings) settings.Settings {
	cfg := set
	if cfg.Credentials == nil {
		cfg.Credentials = make(map[string]map[string]settings.CredentialConfig)
	}
	if cfg.Models == nil {
		cfg.Models = map[string]settings.ModelConfig{}
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]settings.ServiceConfig{}
	}
	// Providers: grouped form → flat ModelConfig using the alias key
	// (provider.<b64>.model.<b64>, design-providers-grouped-config.md §3.3).
	// Also registers user-friendly keys so default_model and SelectModel
	// never need the raw alias: "{pid}/{mid}" for every model, plus the
	// model's runtime_alias when set. A single-model provider also gets a
	// bare "{pid}" shortcut.
	// Fields inherit from the service; per-model differences override.
	for pid, pc := range cfg.Providers {
		if len(pc.Models) == 0 {
			continue
		}
		// OQ1: a disabled service keeps its config but its models are skipped at
		// expansion — they do not appear in the available model space.
		if pc.Enabled != nil && !*pc.Enabled {
			continue
		}
		api := pc.API
		if api == "" {
			api = "openai" // registry re-derives for known services via applyRegistryDefaults
		}
		cred := settings.CredentialRef{Namespace: pc.Credential.Namespace, Name: pc.Credential.Name}
		if cred.IsZero() {
			if model.IsLocalBaseURL(pc.BaseURL) {
				cred = settings.CredentialRef{} // local service, no credential needed
			} else {
				cred = settings.CredentialRef{Namespace: "llm", Name: pid}
			}
		}
		valid := 0
		var firstAlias string
		for _, pm := range pc.Models {
			if pm.ID == "" {
				continue
			}
			// Per-model api override, otherwise inherit from the provider.
			modelAPI := pm.API
			if modelAPI == "" {
				modelAPI = api
			}
			valid++
			key := aliasKey(pid, pm.ID)
			mc := settings.ModelConfig{
				Name:                key,
				Provider:            modelAPI,
				BaseURL:             pc.BaseURL,
				Model:               pm.ID,
				Temperature:         pm.Temperature,
				ContextWindow:       pm.ContextWindow,
				InputPricePerM:      pm.InputPricePerM,
				OutputPricePerM:     pm.OutputPricePerM,
				CacheInputPricePerM: pm.CacheInputPricePerM,
				Credential:          cred,
				WebSearch:           pm.WebSearch,
				CompactRatio:        pm.CompactRatio,
				Catalog: settings.ModelCatalogMetadata{
					ConnectionID:          pid,
					ProviderID:            pid,
					ConnectionDisplayName: pid,
					DisplayName:           pm.ID,
					SupportsTools:         pm.SupportsTools,
					SupportsReasoning:     pm.SupportsReasoning != nil && *pm.SupportsReasoning,
					InputModalities:       pm.InputModalities,
				},
			}

			cfg.Models[key] = mc
			if valid == 1 {
				firstAlias = key
			}

			// Friendly name: "{pid}/{mid}" — always registered as a
			// human-readable key.
			friendly := pid + "/" + pm.ID
			if _, taken := cfg.Models[friendly]; !taken {
				c := mc
				c.Name = friendly
				cfg.Models[friendly] = c
			}

			// runtime_alias: a short name the user chose for this model.
			if pm.RuntimeAlias != "" {
				if _, taken := cfg.Models[pm.RuntimeAlias]; !taken {
					c := mc
					c.Name = pm.RuntimeAlias
					cfg.Models[pm.RuntimeAlias] = c
				}
			}
		}
		// Provider shortcut: bare "{pid}" maps to the first model of the
		// provider. For single-model providers this is the obvious mapping;
		// for multi-model providers it picks the first model as the default
		// (e.g. "deepseek" → the first model under providers.deepseek).
		// An explicit runtime_alias on a model takes precedence over this
		// shortcut (the !taken guard ensures the alias wins).
		if valid >= 1 {
			if _, taken := cfg.Models[pid]; !taken {
				c := cfg.Models[firstAlias]
				c.Name = pid
				cfg.Models[pid] = c
			}
		}
	}
	// Credentials: namespace → name → config.
	for ns, entries := range set.Credentials {
		cfg.Credentials[ns] = make(map[string]settings.CredentialConfig, len(entries))
		for name, cc := range entries {
			cfg.Credentials[ns][name] = settings.CredentialConfig{Source: cc.Source, Env: cc.Env}
		}
	}
	// Agent / Provider / Web / Runtime: field-level copy. VerifyCommand stays
	// empty here — the finalize-verify command is resolved from settings.Verify
	// by the runtime (settings.ResolveVerifyFrom); Config no longer carries it.
	// SubagentModel falls back to the top-level set.SubagentModel (hand-authored
	// settings.json may put it there instead of under agent).
	subagentModel := set.Agent.SubagentModel
	if subagentModel == "" {
		subagentModel = set.SubagentModel
	}
	cfg.SubagentModel = subagentModel

	// Apply the shared normalization pass (registry fill, defaults, credential-ref
	// derivation, default_model fallback) so a settings-sourced Config behaves
	// identically to one parsed from config.yaml.
	if err := normalizeConfig(&cfg); err != nil {
		return cfg
	}
	return cfg
}

// LoadFromSettings builds a fully-normalized Config from the merged
// settings view. It is the settings-first counterpart to LoadConfigLayered:
// CLI/daemon entry points call this instead of reading config.yaml, so
// infrastructure (models/credentials/agent/provider/web) comes from
// settings.json while code-level fields (MCP, GlobalSkillsDir, StoreFactory…)
// remain zero and are assembled by the caller. FromSettings + normalizeConfig
// keep behavior identical to the YAML path.
func LoadFromSettings(set settings.Settings) settings.Settings {
	return FromSettings(set)
}

// LoadSettingsBytes parses configuration from raw YAML bytes (nil or empty =>
// built-in defaults), applying the same normalization and validation as
// LoadConfig. Embedded hosts (iOS/macOS in-app) supply config in-memory rather
// than from a file path, since the app sandbox has no fixed config.yaml.
func LoadSettingsBytes(data []byte) (settings.Settings, error) {
	cfg := settings.Settings{
		Agent:  settings.AgentConfig{MaxSteps: 8},
		Server: settings.ServerConfig{PublicHealthz: true},
	}

	if len(data) > 0 {
		var set settings.File
		if err := json.Unmarshal(data, &set); err != nil {
			err = yaml.Unmarshal(data, &set)
			if err != nil {
				return cfg, err
			}
		}
		cfg = set.ToSettings()
		cfg = FromSettings(cfg)
	}

	// Backward compatibility applies only when the models field is absent. An
	// explicit models: {} is the host's zero-Provider read-only mode and must
	// remain empty.
	if cfg.Models == nil {
		cfg.Models = map[string]settings.ModelConfig{
			"deepseek": {
				Provider: "openai",
				BaseURL:  "https://api.deepseek.com",
				Model:    "deepseek-v4-flash",
			},
		}
	}
	if err := normalizeConfig(&cfg); err != nil {
		return settings.Settings{}, err
	}
	return cfg, nil
}

// normalizeConfig applies the shared normalization pass to a Config regardless
// of its source (config.yaml YAML or settings.json via FromSettings): model
// defaults + registry fill + credential-ref derivation, default_model fallback,
// agent/provider/web/runtime defaults, and gateway web-search wiring.
//
// The api_key_env deprecation warning is deliberately emitted here for BOTH
// sources: a model declared in settings.json with an api_key_env field is the
// same legacy path and should be flagged identically.
func normalizeConfig(cfg *settings.Settings) error {
	if len(cfg.Models) > 0 && cfg.DefaultModel == "" {
		names := cfg.ModelNames()
		cfg.DefaultModel = names[0]
	}

	// Resolve per-model defaults and API keys. Missing keys are NOT an error
	// here; they are reported only when a model is actually selected.
	for name, mc := range cfg.Models {
		// design-providers-grouped-config.md §3.1: flat model keys must be
		// slash-free (alias keys from providers expansion already are). Keys
		// generated by the providers expansion (alias + friendly "{pid}/{mid}")
		// carry a Catalog.ConnectionID and legitimately contain "/", so they are
		// exempt; only user-authored flat keys are validated.
		if mc.Catalog.ConnectionID == "" {
			if err := validateFlatModelKey(name); err != nil {
				return err
			}
		}
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
				mc.Credential = settings.CredentialRef{Namespace: "llm", Name: name}
			} else if model.IsLocalBaseURL(mc.BaseURL) {
				mc.Credential = settings.CredentialRef{} // none needed
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
	// Default each model's compact ratio to the agent-level ratio unless the
	// model declares its own (model-level wins, matching settingsFileFrom).
	// Without this, the default-model path (resolveTurnModel with an empty
	// request) never goes through SelectModel and would carry CompactRatio 0,
	// zeroing every session's compaction threshold.
	for name, mc := range cfg.Models {
		if mc.CompactRatio <= 0 {
			mc.CompactRatio = cfg.Agent.CompactRatio
		}
		cfg.Models[name] = mc
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
		return fmt.Errorf("default_model %q cannot be set when models is empty", cfg.DefaultModel)
	}

	if cfg.Web.Search.Provider == "gateway" {
		if cfg.Web.Search.Credential.IsZero() {
			cfg.Web.Search.Credential = settings.CredentialRef{Namespace: "gateway", Name: "default"}
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

	return nil
}

// SelectModel resolves a model by friendly name (empty name => default_model).
// It fails if the model is unknown or its API key is not set.
func SelectModel(name string, set settings.Settings) (settings.ModelConfig, error) {
	if name == "" {
		name = set.DefaultModel
	}
	// R2.3: fall back to the built-in registry for known connection names so
	// `--model deepseek` / subagent_model: deepseek keep working even when
	// the config never declared them (the model/credential config is
	// user-level; the registry provides the open-box default). Unknown names
	// still error exactly as before.
	//conn, known := builtinConnections[name]
	mc, known := set.Models[name]
	if !known {
		return settings.ModelConfig{}, fmt.Errorf("SelectModel unknown model %s", name)
	}

	if mc.Temperature <= 0 {
		mc.Temperature = 0.2
	}
	if mc.ContextWindow <= 0 {
		mc.ContextWindow = defaultContextWindow
	}
	// Normalize already defaulted CompactRatio to the agent ratio; this only
	// covers ModelConfigs that bypassed normalization (e.g. builtin connections).
	if mc.CompactRatio <= 0 {
		mc.CompactRatio = set.Agent.CompactRatio
	}
	if mc.CompactRatio <= 0.0 {
		mc.CompactRatio = defaultCompactRatio
	}
	if mc.APIKeyEnv == "" && !model.IsLocalBaseURL(mc.BaseURL) && mc.Credential.IsZero() {
		// The gateway connection is a declaration, not a concrete model: no
		// fixed endpoint and no env key — its credential is resolved at call
		// time from the injected resolver (embedded /v1/secrets, CLI
		// --gateway-token). A zero Credential here is legitimate; only reject
		// concrete models that genuinely lack a configured credential.
		if conn, known := builtinConnections[name]; !(known && conn.Kind == "gateway") {
			return settings.ModelConfig{}, fmt.Errorf("model %q has no credential configured; add a credential: section or set api_key_env", name)
		}
	}
	return mc, nil
}
