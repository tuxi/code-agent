package settings

import "testing"

func TestModelConfigSupportsVision(t *testing.T) {
	tests := []struct {
		name       string
		modalities []string
		want       bool
	}{
		{"no modalities declared", nil, false},
		{"text only", []string{"text"}, false},
		{"text and image", []string{"text", "image"}, true},
		{"image only", []string{"image"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: tc.modalities}}
			if got := mc.SupportsVision(); got != tc.want {
				t.Errorf("SupportsVision() = %v, want %v", got, tc.want)
			}
		})
	}
}
