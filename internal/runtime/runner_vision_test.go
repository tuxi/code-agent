package runtime

import (
	"encoding/json"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

// TestBuildRunnerDerivesVisionSupported verifies the runner's vision capability
// is derived from the model config's declared input modalities AND the wire
// transport being able to serialize content parts (OpenAI-compatible only).
// Transport-level Gateway asset upload stays separate.
func TestBuildRunnerDerivesVisionSupported(t *testing.T) {
	cases := []struct {
		name string
		mc   settings.ModelConfig
		want bool
	}{
		{"text only", settings.ModelConfig{Model: "m", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text"}}}, false},
		{"vision openai", settings.ModelConfig{Model: "m", Provider: "openai", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}}}, true},
		{"vision default provider", settings.ModelConfig{Model: "m", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}}}, true},
		{"vision ollama transport", settings.ModelConfig{Model: "m", Provider: "ollama", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}}}, false},
		{"vision responses transport", settings.ModelConfig{Model: "m", Provider: "responses", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}}}, true},
		{"no modalities", settings.ModelConfig{Model: "m"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := BuildRunner(settings.Settings{}, tc.mc, &model.OpenAICompatibleProvider{}, tools.NewRegistry(), nil, allowApprover{}, nil, nil, t.TempDir())
			if r.VisionSupported != tc.want {
				t.Errorf("VisionSupported = %v, want %v", r.VisionSupported, tc.want)
			}
			// The transport-level capability must not leak into the vision flag.
			if r.UserAssetsSupported {
				t.Errorf("UserAssetsSupported = true, want false for a direct provider")
			}
		})
	}
}

// allowApprover approves every tool call without prompting (test helper).
type allowApprover struct{}

func (allowApprover) Approve(string, json.RawMessage) agent.Verdict { return agent.VerdictAllow }
