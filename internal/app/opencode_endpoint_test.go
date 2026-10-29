package app

import (
	"testing"

	"code-agent/internal/settings"
)

func TestOpenCodeGoEndpointDefaults(t *testing.T) {
	const canonical = "https://opencode.ai/inference/go/openai/v1"
	for _, configured := range []string{"", "https://opencode.ai/zen/go/v1", "https://proxy.example/v1"} {
		t.Run(configured, func(t *testing.T) {
			cfg, err := FromSettings(settings.Settings{Providers: map[string]settings.ServiceConfig{
				"opencode-go": {BaseURL: configured, Models: []settings.ProviderModel{{ID: "gpt-5.6-luna", API: "responses"}, {ID: "deepseek-v4.1-flash"}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			want := configured
			if want == "" {
				want = canonical
			}
			for _, id := range []string{"gpt-5.6-luna", "deepseek-v4.1-flash"} {
				mc := cfg.Models[aliasKey("opencode-go", id)]
				if mc.BaseURL != want {
					t.Errorf("%s base_url = %q, want %q", id, mc.BaseURL, want)
				}
			}
		})
	}
}
