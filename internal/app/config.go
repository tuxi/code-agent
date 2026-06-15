package app

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Model     ModelConfig     `yaml:"model"`
	Agent     AgentConfig     `yaml:"agent"`
	Workspace WorkspaceConfig `yaml:"workspace"`
}

type ModelConfig struct {
	Provider    string  `yaml:"provider"`
	BaseURL     string  `yaml:"base_url"`
	Model       string  `yaml:"model"`
	APIKey      string  `yaml:"-"`
	Temperature float64 `yaml:"temperature"`
}

type AgentConfig struct {
	MaxSteps int `yaml:"max_steps"`
}

type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

func DefaultConfig() Config {
	return Config{
		Model: ModelConfig{
			Provider:    "deepseek",
			BaseURL:     "https://api.deepseek.com",
			Model:       "deepseek-v4-flash",
			Temperature: 0.2,
		},
		Agent: AgentConfig{
			MaxSteps: 8,
		},
		Workspace: WorkspaceConfig{
			Root: ".",
		},
	}
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

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

	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("CODEAGENT_API_KEY")
	}
	cfg.Model.APIKey = apiKey

	if cfg.Model.BaseURL == "" {
		cfg.Model.BaseURL = "https://api.deepseek.com"
	}
	if cfg.Model.Model == "" {
		cfg.Model.Model = "deepseek-v4-flash"
	}
	if cfg.Agent.MaxSteps <= 0 {
		cfg.Agent.MaxSteps = 8
	}
	if cfg.Workspace.Root == "" {
		cfg.Workspace.Root = "."
	}

	if cfg.Model.APIKey == "" {
		return Config{}, errors.New("missing DEEPSEEK_API_KEY")
	}

	return cfg, nil
}
