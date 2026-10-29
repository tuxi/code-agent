package settings

import (
	"code-agent/internal/credential"
	"context"
	"testing"
	"time"
)

func TestModelAvailabilityRequiresEffectiveCredential(t *testing.T) {
	target := credential.Target{Namespace: "llm", Name: "private"}
	mc := ModelConfig{Name: "corp/vendor/model", Provider: "openai", BaseURL: "https://example.invalid/v1", Model: "vendor/model", Credential: CredentialRef{Namespace: target.Namespace, Name: target.Name}, Catalog: ModelCatalogMetadata{ConnectionID: "corp"}}
	cfg := Settings{Credentials: map[string]map[string]CredentialConfig{"llm": {"private": {Source: "injected"}}}}
	if id := ModelID(mc.Name, mc); id != "corp/vendor/model" {
		t.Fatal(id)
	}
	for _, entry := range []struct {
		name string
		c    credential.Credential
		want bool
	}{
		{"missing", credential.Credential{}, false},
		{"empty bearer", credential.Credential{Type: credential.Bearer}, false},
		{"injected present", credential.Credential{Type: credential.Bearer, Secret: "test-only"}, true},
		{"no auth", credential.Credential{Type: credential.None}, true},
	} {
		t.Run(entry.name, func(t *testing.T) {
			ok, _, _, _ := ProbeModelAvailability(context.Background(), cfg, mc, credential.StaticResolver{target: entry.c})
			if ok != entry.want {
				t.Fatalf("available=%v", ok)
			}
		})
	}
	expired := time.Now().Add(-time.Second)
	if ok, _, _, _ := ProbeModelAvailability(context.Background(), cfg, mc, credential.StaticResolver{target: {Type: credential.Bearer, Secret: "expired", ExpiresAt: &expired}}); ok {
		t.Fatal("expired credential available")
	}
	before := ModelConfigurationID(mc)
	mc.BaseURL = "https://another.invalid/v1"
	if before == ModelConfigurationID(mc) {
		t.Fatal("endpoint change reused verification identity")
	}
}
