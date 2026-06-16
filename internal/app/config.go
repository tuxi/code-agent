package app

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// defaultContextWindow is assumed for any model that does not declare its own
	// context_window. 128k matches the smaller models currently configured.
	defaultContextWindow = 128000
	// defaultCompactRatio is the fraction of the context window at which a session
	// compacts when agent.compact_ratio is unset or invalid.
	defaultCompactRatio = 0.7

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
}

type WorkspaceConfig struct {
	Root string `yaml:"root"`
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
	if cfg.Workspace.Root == "" {
		cfg.Workspace.Root = "."
	}

	if _, ok := cfg.Models[cfg.DefaultModel]; !ok {
		return Config{}, fmt.Errorf("default_model %q is not defined under models", cfg.DefaultModel)
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
	if mc.APIKey == "" {
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
