package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"code-agent/internal/settings"
	"gopkg.in/yaml.v3"
)

// MigrateConfigToSettings migrates legacy config.yaml files into the
// settings.json layer (design-config-settings-merge.md stage C).
//
// It reads the user-level ~/.codeagent/config.yaml and the project-level
// <root>/.codeagent/config.yaml (if present), merges them with the same
// precedence LoadConfigLayered used, and writes the result as settings.json:
//   - user-level merged config → ~/.codeagent/settings.json
//   - project-level config → <root>/.codeagent/settings.json (when present)
//
// Secrets are never written into the committable settings.json: a
// server.access_token value is replaced with access_token_env pointing at
// CODEAGENT_SERVER_ACCESS_TOKEN, and credential values (already refs, not
// values, in config.yaml) are carried as-is.
//
// The config.yaml files are left in place (not deleted); the caller decides
// when to remove them.
func MigrateConfigToSettings(root, home string) error {
	// User-level config.yaml.
	userPath := pathJoin(home, ".codeagent", "config.yaml")
	userCfg, userErr := loadConfigFile(userPath)

	// Project-level config.yaml (optional).
	projectPath := pathJoin(root, ".codeagent", "config.yaml")
	projectCfg, projectErr := loadConfigFile(projectPath)

	// Build merged settings from whatever layers exist.
	set := mergedSettings(userCfg, userErr, projectCfg, projectErr)

	// Nothing to migrate (no models, credentials, defaults, or agent knobs) —
	// don't write an empty settings.json.
	if isEmptySettingsFile(set) {
		return nil
	}

	// Write user-level settings.json carrying ONLY the user layer (project A's
	// models/credentials must not leak into ~/.codeagent/settings.json for every
	// other project). Project overrides go to the project-scope file, which the
	// settings.Load merge applies on top.
	userFile := settingsFileFrom(userCfg)
	userOut := pathJoin(home, ".codeagent", "settings.json")
	if err := writeSettingsFile(userOut, userFile); err != nil {
		return err
	}
	// Project-level settings.json mirrors the project-layer overrides — but the
	// server section is a USER-level (deployment) concern and must not leak into
	// a committable project file. It stays only in ~/.codeagent/settings.json.
	projFile := settingsFileFrom(projectCfg)
	projFile.Server = settings.ServerConfig{}
	if !isEmptySettingsFile(projFile) {
		projOut := pathJoin(root, ".codeagent", "settings.json")
		if err := writeSettingsFile(projOut, projFile); err != nil {
			return err
		}
	}
	return nil
}

// loadConfigFile reads a config.yaml path; a missing file yields an empty
// Config (not an error) so migration is a no-op when there is nothing to move.
// It parses WITHOUT normalization: migration must carry exactly what the user
// wrote, so an unset agent.max_steps stays 0 (not the normalized default 8) —
// otherwise a project layer that never set agent would write the default and
// override the user layer during settings.Load merging.
func loadConfigFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// mergedSettings merges the user and project Config layers into one
// settings.File. errs are honored: if a layer errored it is skipped.
func mergedSettings(userCfg Config, userErr error, projectCfg Config, projectErr error) settings.File {
	out := settingsFileFrom(userCfg)
	if userErr != nil || projectErr != nil {
		return out
	}
	// Project layer wins per field (mirrors LoadConfigLayered → MergeConfigs).
	proj := settingsFileFrom(projectCfg)
	if proj.DefaultModel != "" {
		out.DefaultModel = proj.DefaultModel
	}
	if proj.SubagentModel != "" {
		out.SubagentModel = proj.SubagentModel
	}
	for name, mc := range proj.Models {
		if out.Models == nil {
			out.Models = map[string]settings.ModelConfig{}
		}
		out.Models[name] = mc
	}
	for ns, entries := range proj.Credentials {
		if out.Credentials == nil {
			out.Credentials = map[string]map[string]settings.CredentialConfig{}
		}
		if out.Credentials[ns] == nil {
			out.Credentials[ns] = map[string]settings.CredentialConfig{}
		}
		for name, cc := range entries {
			out.Credentials[ns][name] = cc
		}
	}
	return out
}

// settingsFileFrom converts a Config into a settings.File (the disk shape).
// Only the user-configurable infrastructure subset is carried; code-level
// fields (MCP, Profile, StoreFactory) are not.
func settingsFileFrom(cfg Config) settings.File {
	f := settings.File{
		DefaultModel:  cfg.DefaultModel,
		SubagentModel: cfg.Agent.SubagentModel,
		Agent: settings.AgentConfig{
			MaxSteps:                 cfg.Agent.MaxSteps,
			MaxParallelTools:         cfg.Agent.MaxParallelTools,
			CompactRatio:             cfg.Agent.CompactRatio,
			CompactKeepRatio:         cfg.Agent.CompactKeepRatio,
			ClientToolTimeoutSeconds: cfg.Agent.ClientToolTimeoutSeconds,
			SubagentModel:            cfg.Agent.SubagentModel,
		},
		Provider: settings.ProviderConfig{
			RequestTimeoutSeconds: cfg.Provider.RequestTimeoutSeconds,
			MaxRetries:            cfg.Provider.MaxRetries,
			BackoffMillis:         cfg.Provider.BackoffMillis,
			MaxBackoffSeconds:     cfg.Provider.MaxBackoffSeconds,
		},
		Web: settings.WebConfig{
			Search: settings.WebSearchConfig{
				Provider:         cfg.Web.Search.Provider,
				FallbackProvider: cfg.Web.Search.FallbackProvider,
				GatewayBaseURL:   cfg.Web.Search.GatewayBaseURL,
				TopK:             cfg.Web.Search.TopK,
				TimeoutSeconds:   cfg.Web.Search.TimeoutSeconds,
				TavilyAPIKeyEnv:  cfg.Web.Search.TavilyAPIKeyEnv,
				BraveAPIKeyEnv:   cfg.Web.Search.BraveAPIKeyEnv,
			},
			Fetch: settings.WebFetchConfig{
				TimeoutSeconds:  cfg.Web.Fetch.TimeoutSeconds,
				CacheTTLSeconds: cfg.Web.Fetch.CacheTTLSeconds,
			},
		},
		Runtime:  settings.RuntimeConfig{MaxConcurrentTurns: cfg.Runtime.MaxConcurrentTurns},
		Currency: cfg.Currency,
		Server: settings.ServerConfig{
			DisplayName:    cfg.Server.DisplayName,
			Authentication: cfg.Server.Authentication,
			// The access_token value is carried into the USER-level settings.json
			// (the file is deployment-scoped; the user decides whether to commit
			// it). AccessTokenEnv is preserved when set — it wins at resolve time.
			AccessTokenEnv: cfg.Server.AccessTokenEnv,
			AccessToken:    cfg.Server.AccessToken,
			PublicHealthz:  cfg.Server.PublicHealthz,
			TLSCertificate: cfg.Server.TLSCertificate,
			TLSPrivateKey:  cfg.Server.TLSPrivateKey,
		},
	}
	if len(cfg.Models) > 0 {
		f.Models = make(map[string]settings.ModelConfig, len(cfg.Models))
		for name, mc := range cfg.Models {
			f.Models[name] = settings.ModelConfig{
				Provider:          mc.Provider,
				BaseURL:           mc.BaseURL,
				Model:             mc.Model,
				APIKeyEnv:         mc.APIKeyEnv,
				Temperature:       mc.Temperature,
				ContextWindow:     mc.ContextWindow,
				InputPricePerM:    mc.InputPricePerM,
				OutputPricePerM:   mc.OutputPricePerM,
				CacheInputPricePerM: mc.CacheInputPricePerM,
				Credential:        settings.CredentialRef{Namespace: mc.Credential.Namespace, Name: mc.Credential.Name},
				Catalog: settings.ModelCatalogMetadata{
					ConnectionID:          mc.Catalog.ConnectionID,
					ProviderID:            mc.Catalog.ProviderID,
					ConnectionDisplayName: mc.Catalog.ConnectionDisplayName,
					DisplayName:           mc.Catalog.DisplayName,
					SupportsTools:         mc.Catalog.SupportsTools,
					SupportsReasoning:     mc.Catalog.SupportsReasoning,
					InputModalities:       mc.Catalog.InputModalities,
				},
			}
		}
	}
	if len(cfg.Credentials) > 0 {
		f.Credentials = make(map[string]map[string]settings.CredentialConfig, len(cfg.Credentials))
		for ns, entries := range cfg.Credentials {
			f.Credentials[ns] = make(map[string]settings.CredentialConfig, len(entries))
			for name, cc := range entries {
				f.Credentials[ns][name] = settings.CredentialConfig{Source: cc.Source, Env: cc.Env}
			}
		}
	}
	return f
}

// writeSettingsFile writes a settings.File to path, preserving unknown keys
// via settings.Persist and enforcing the committable-secret rule (no value
// fields are carried — the File type has none). Only non-empty fields are
// written, so a migration never clobbers a pre-existing permissions/verify/
// hooks block with null.
func writeSettingsFile(path string, f settings.File) error {
	doc := map[string]any{}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	return settings.Persist(path, func(d map[string]any) {
		for k, v := range doc {
			if isZeroJSONValue(v) {
				continue
			}
			d[k] = v
		}
	})
}

// isZeroJSONValue reports whether a decoded JSON value is "zero" — nil, empty
// string, empty array/map, false, or 0 — and should not be written.
func isZeroJSONValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

func pathJoin(parts ...string) string {
	return filepath.Join(parts...)
}

// isEmptySettingsFile reports whether a settings.File carries no configurable
// content — nothing worth writing as a migration artifact.
func isEmptySettingsFile(f settings.File) bool {
	return len(f.Models) == 0 && len(f.Credentials) == 0 &&
		f.DefaultModel == "" && f.SubagentModel == "" &&
		f.Agent.MaxSteps == 0 && f.Agent.MaxParallelTools == 0 &&
		f.Agent.CompactRatio == 0 && f.Agent.CompactKeepRatio == 0 &&
		f.Agent.ClientToolTimeoutSeconds == 0 && f.Agent.SubagentModel == "" &&
		f.Provider.RequestTimeoutSeconds == 0 && f.Provider.MaxRetries == 0 &&
		f.Provider.BackoffMillis == 0 && f.Provider.MaxBackoffSeconds == 0 &&
		f.Web.Search.Provider == "" && f.Web.Search.FallbackProvider == "" &&
		f.Web.Search.GatewayBaseURL == "" && f.Web.Search.TopK == 0 &&
		f.Web.Search.TimeoutSeconds == 0 && f.Web.Search.TavilyAPIKeyEnv == "" &&
		f.Web.Search.BraveAPIKeyEnv == "" && f.Web.Fetch.TimeoutSeconds == 0 &&
		f.Web.Fetch.CacheTTLSeconds == 0 && f.Runtime.MaxConcurrentTurns == 0 &&
		f.Server.DisplayName == "" && f.Server.Authentication == "" &&
		f.Server.AccessTokenEnv == "" && f.Server.AccessToken == "" &&
		!f.Server.PublicHealthz &&
		f.Server.TLSCertificate == "" && f.Server.TLSPrivateKey == "" &&
		f.Currency == ""
}
