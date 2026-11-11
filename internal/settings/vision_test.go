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

func TestModelConfigSupportsToolImageInput(t *testing.T) {
	trueValue := true
	falseValue := false
	tests := []struct {
		name string
		mc   ModelConfig
		want bool
	}{
		{"text only", ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: []string{"text"}}}, false},
		{"image default", ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: []string{"image"}}}, true},
		{"image explicit true", ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: []string{"image"}, ToolImageInput: &trueValue}}, true},
		{"image explicit false", ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: []string{"image"}, ToolImageInput: &falseValue}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mc.SupportsToolImageInput(); got != tc.want {
				t.Fatalf("SupportsToolImageInput() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestModelConfigSupportsOriginalImageDetail(t *testing.T) {
	trueValue := true
	falseValue := false
	tests := []struct {
		name string
		mc   ModelConfig
		want bool
	}{
		{"unset", ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: []string{"image"}}}, false},
		{"explicit false", ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: []string{"image"}, ImageDetailOriginal: &falseValue}}, false},
		{"explicit true", ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: []string{"image"}, ImageDetailOriginal: &trueValue}}, true},
		{"text model explicit true", ModelConfig{Catalog: ModelCatalogMetadata{InputModalities: []string{"text"}, ImageDetailOriginal: &trueValue}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mc.SupportsOriginalImageDetail(); got != tc.want {
				t.Fatalf("SupportsOriginalImageDetail() = %v, want %v", got, tc.want)
			}
		})
	}
}
