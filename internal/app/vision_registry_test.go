package app

import (
	"testing"

	"code-agent/internal/settings"
)

// TestFromSettingsVisionModalityFlowsThrough verifies a grouped provider's
// per-model input_modalities reaches the flat ModelConfig catalog and drives
// SupportsVision — the config-only path to enabling a multimodal model.
func TestFromSettingsVisionModalityFlowsThrough(t *testing.T) {
	sf, err := settings.ParseJSON([]byte(`{
  "providers": {
    "deepseek": {
      "models": [
        {"id": "deepseek-v4-flash"},
        {"id": "deepseek-v4-flash-vision-exp", "input_modalities": ["text", "image"]}
      ]
    }
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := FromSettings(settings.Settings{Providers: sf.Providers})
	if err != nil {
		t.Fatal(err)
	}
	textKey := aliasKey("deepseek", "deepseek-v4-flash")
	visionKey := aliasKey("deepseek", "deepseek-v4-flash-vision-exp")
	if cfg.Models[textKey].SupportsVision() {
		t.Errorf("deepseek-v4-flash SupportsVision() = true, want false (no image modality declared)")
	}
	if !cfg.Models[visionKey].SupportsVision() {
		t.Errorf("deepseek-v4-flash-vision-exp SupportsVision() = false, want true")
	}
}

// TestDeepseekTemplateIncludesVisionModel verifies the built-in deepseek
// connection advertises the image-capable vision model in provider templates.
func TestDeepseekTemplateIncludesVisionModel(t *testing.T) {
	templates := BuiltinProviderTemplates()
	var conn *BuiltinProviderTemplate
	for i := range templates {
		if templates[i].ID == "deepseek" {
			conn = &templates[i]
			break
		}
	}
	if conn == nil {
		t.Fatal("deepseek builtin connection missing from templates")
	}
	var found bool
	for _, m := range conn.Models {
		if m.ID == "deepseek-v4-flash-vision-exp" {
			found = true
			if !hasModality(m.InputModalities, "image") {
				t.Errorf("vision model modalities = %v, want image included", m.InputModalities)
			}
		}
	}
	if !found {
		t.Fatalf("deepseek template models missing deepseek-v4-flash-vision-exp: %+v", conn.Models)
	}
}

func hasModality(modalities []string, want string) bool {
	for _, m := range modalities {
		if m == want {
			return true
		}
	}
	return false
}
