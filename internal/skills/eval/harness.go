// Package eval provides a lightweight behavioral harness for skill evaluation.
// It runs prompt/skill pairs against a real model and checks responses for
// expected (and forbidden) markers — a correctness regression suite that catches
// skill drift.
//
// Tests are gated behind testing.Short() so they only run when explicitly
// requested: go test -short=false ./internal/skills/eval/...
package eval

import (
	"code-agent/internal/app"
	"code-agent/internal/model"
	"code-agent/internal/runtime"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Case is a single eval test case.
type Case struct {
	Name        string   `json:"name"`
	Skill       string   `json:"skill"`
	Prompt      string   `json:"prompt"`
	MustContain []string `json:"must_contain"`
	MustNot     []string `json:"must_not_contain"`
	Description string   `json:"description"`
}

// LoadCases reads eval cases from a JSON file.
func LoadCases(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read evals file: %w", err)
	}
	var cases []Case
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, fmt.Errorf("parse evals file: %w", err)
	}
	return cases, nil
}

// Harness runs eval cases against a real model.
type Harness struct {
	Provider    model.Provider
	ModelName   string
	Temperature float64
	SkillsDir   string // path to a directory containing skill fixtures
}

// NewHarness creates a Harness from the project's config.yaml. It looks for
// config.yaml in the project root (two levels above internal/skills/eval).
func NewHarness() (*Harness, error) {
	// Find the project root relative to this package.
	root, err := findProjectRoot()
	if err != nil {
		return nil, err
	}
	cfg, err := app.LoadConfig(filepath.Join(root, "config.yaml"))
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	mc, err := cfg.SelectModel("")
	if err != nil {
		return nil, fmt.Errorf("select model: %w", err)
	}
	provider, err := runtime.BuildProvider(mc, cfg.Provider, nil)
	if err != nil {
		return nil, fmt.Errorf("build provider: %w", err)
	}
	return &Harness{
		Provider:    provider,
		ModelName:   mc.Model,
		Temperature: mc.Temperature,
		SkillsDir:   filepath.Join(root, "skills"),
	}, nil
}

// Run executes a single eval case and returns a result.
func (h *Harness) Run(ctx context.Context, c Case) EvalResult {
	// Build the system prompt with the skill body injected — a minimal version
	// of what the real agent does via load_skill + PromptIndex.
	system := buildSystemPrompt(c.Skill, h.SkillsDir)

	req := model.Request{
		Model:       h.ModelName,
		Temperature: h.Temperature,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: system},
			{Role: model.RoleUser, Content: c.Prompt},
		},
	}

	start := time.Now()
	resp, err := h.Provider.Complete(ctx, req)
	elapsed := time.Since(start)

	result := EvalResult{
		Case:    c,
		Elapsed: elapsed,
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Response = resp.Content

	// Check markers.
	for _, marker := range c.MustContain {
		if !strings.Contains(strings.ToLower(resp.Content), strings.ToLower(marker)) {
			result.Missing = append(result.Missing, marker)
		}
	}
	for _, marker := range c.MustNot {
		if strings.Contains(strings.ToLower(resp.Content), strings.ToLower(marker)) {
			result.Forbidden = append(result.Forbidden, marker)
		}
	}
	result.Passed = len(result.Missing) == 0 && len(result.Forbidden) == 0 && result.Error == ""
	return result
}

// EvalResult captures the outcome of a single eval run.
type EvalResult struct {
	Case      Case
	Response  string
	Error     string
	Missing   []string // must_contain markers not found
	Forbidden []string // must_not_contain markers found
	Elapsed   time.Duration
	Passed    bool
}

// buildSystemPrompt creates a minimal system prompt that includes the skill's
// full body, simulating what happens after load_skill is called.
func buildSystemPrompt(skillName, skillsDir string) string {
	body := ""
	p := filepath.Join(skillsDir, skillName, "SKILL.md")
	if data, err := os.ReadFile(p); err == nil {
		body = string(data)
	}
	return fmt.Sprintf(`You are a coding assistant. The user has loaded the skill "%s".

=== SKILL: %s ===
%s
=== END SKILL ===

Follow the skill's instructions when they apply to the task. Be concise.`,
		skillName, skillName, body)
}

// findProjectRoot walks up from the current package's directory to find the
// project root (the directory containing go.mod).
func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("project root (go.mod) not found")
		}
		dir = parent
	}
}
