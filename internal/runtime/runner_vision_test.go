package runtime

import (
	"encoding/json"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

// TestBuildRunnerDerivesImageCapabilities verifies the runner's image capabilities
// is derived from the model config's declared input modalities AND the wire
// transport being able to serialize content parts (OpenAI-compatible only).
// Transport-level Gateway asset upload stays separate.
func TestBuildRunnerDerivesImageCapabilities(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	cases := []struct {
		name              string
		mc                settings.ModelConfig
		wantVision        bool
		wantToolImage     bool
		wantOriginalImage bool
	}{
		{"text only", settings.ModelConfig{Model: "m", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text"}}}, false, false, false},
		{"vision openai", settings.ModelConfig{Model: "m", Provider: "openai", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}}}, true, true, false},
		{"vision default provider", settings.ModelConfig{Model: "m", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}}}, true, true, false},
		{"vision ollama transport", settings.ModelConfig{Model: "m", Provider: "ollama", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}}}, false, false, false},
		{"vision responses transport", settings.ModelConfig{Model: "m", Provider: "responses", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}}}, true, true, false},
		{"tool image denied", settings.ModelConfig{Model: "m", Provider: "openai", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}, ToolImageInput: boolPtr(false)}}, true, false, false},
		{"original detail", settings.ModelConfig{Model: "m", Provider: "openai", Catalog: settings.ModelCatalogMetadata{InputModalities: []string{"text", "image"}, ImageDetailOriginal: boolPtr(true)}}, true, true, true},
		{"no modalities", settings.ModelConfig{Model: "m"}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := BuildRunner(settings.Settings{}, tc.mc, &model.OpenAICompatibleProvider{}, tools.NewRegistry(), nil, allowApprover{}, nil, nil, t.TempDir())
			if r.VisionSupported != tc.wantVision {
				t.Errorf("VisionSupported = %v, want %v", r.VisionSupported, tc.wantVision)
			}
			if r.ToolImageInputSupported != tc.wantToolImage {
				t.Errorf("ToolImageInputSupported = %v, want %v", r.ToolImageInputSupported, tc.wantToolImage)
			}
			if r.ImageDetailOriginalSupported != tc.wantOriginalImage {
				t.Errorf("ImageDetailOriginalSupported = %v, want %v", r.ImageDetailOriginalSupported, tc.wantOriginalImage)
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
