package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigMultiModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yaml := `
default_model: qwen
models:
  deepseek:
    provider: openai
    base_url: https://api.deepseek.com
    model: deepseek-v4-pro
    api_key_env: DEEPSEEK_API_KEY
  qwen:
    provider: openai
    base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
    model: qwen3-coder-plus
    api_key_env: DASHSCOPE_API_KEY
agent:
  max_steps: 12
workspace:
  root: .
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Make key presence deterministic regardless of the caller's environment.
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("DASHSCOPE_API_KEY", "test-key")

	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModel != "qwen" {
		t.Errorf("default_model = %q, want qwen", cfg.DefaultModel)
	}
	if cfg.Agent.MaxSteps != 12 {
		t.Errorf("max_steps = %d, want 12", cfg.Agent.MaxSteps)
	}

	// Default selection (qwen) has its key set -> succeeds.
	mc, err := cfg.SelectModel("")
	if err != nil {
		t.Fatalf("select default: %v", err)
	}
	if mc.Name != "qwen" || mc.Model != "qwen3-coder-plus" {
		t.Errorf("selected %q/%q, want qwen/qwen3-coder-plus", mc.Name, mc.Model)
	}
	if mc.Temperature != 0.2 {
		t.Errorf("temperature = %v, want default 0.2", mc.Temperature)
	}

	// deepseek is configured but its key is unset -> selection fails clearly.
	if _, err := cfg.SelectModel("deepseek"); err == nil {
		t.Error("expected an error selecting deepseek with no API key")
	}

	// Unknown model -> error.
	if _, err := cfg.SelectModel("gpt"); err == nil {
		t.Error("expected an error selecting an unknown model")
	}
}

func TestLoadConfigFallsBackToDeepseek(t *testing.T) {
	// No file, no models configured -> built-in deepseek default.
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModel != "deepseek" {
		t.Errorf("default_model = %q, want deepseek", cfg.DefaultModel)
	}
	if _, ok := cfg.Models["deepseek"]; !ok {
		t.Error("expected a built-in deepseek model")
	}
}
