package settings

import "testing"

// ProviderName must return the provider brand, not the wire transport type:
// grouped providers carry their service id in Catalog.ProviderID, legacy
// configs fall back to the friendly config key, and a bare transport type
// (openai/ollama) is returned only when nothing more specific is declared.
func TestModelConfigProviderName(t *testing.T) {
	tests := []struct {
		name string
		mc   ModelConfig
		want string
	}{
		{"grouped service id wins", ModelConfig{Name: "deepseek-v4", Catalog: ModelCatalogMetadata{ProviderID: "deepseek"}}, "deepseek"},
		{"friendly key fallback", ModelConfig{Name: "openrouter"}, "openrouter"},
		{"transport type fallback", ModelConfig{Name: "openai", Provider: "openai"}, "openai"},
		{"empty degrades empty", ModelConfig{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mc.ProviderName(); got != tc.want {
				t.Errorf("ProviderName() = %q, want %q", got, tc.want)
			}
		})
	}
}
