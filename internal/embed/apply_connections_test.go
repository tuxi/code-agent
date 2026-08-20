package embed

import (
	"code-agent/internal/settings"
	"testing"
)

// applyConnections unit tests — cover the credential-ref routing, the
// runtime-alias friendly-name mapping, and the injected credential recording
// that probeModelAvailability relies on.

func TestApplyConnectionsInjectedBYOKUsesLLMNamespace(t *testing.T) {
	cfg := settings.Settings{}
	conns := map[string]connectionDefinition{
		"deepseek": {
			API:     "openai",
			BaseURL: "https://api.deepseek.com",
			Credential: &connectionCredentialDecl{
				Source: "injected",
				Ref:    "deepseek",
			},
			Models: []connectionModelDef{
				{WireModelID: "deepseek-v4-flash"},
			},
		},
	}
	applyConnections(&cfg, conns)

	mc := cfg.Models["deepseek-v4-flash"]
	if mc.Credential != (settings.CredentialRef{Namespace: "llm", Name: "deepseek"}) {
		t.Errorf("credential = %+v, want llm/deepseek", mc.Credential)
	}
	// The injected credential must be recorded in the Credentials section so
	// probeModelAvailability marks it configured.
	if cc, ok := cfg.Credentials["llm"]["deepseek"]; !ok || cc.Source != "injected" {
		t.Errorf("credentials[llm][deepseek] = %+v, want {injected}", cc)
	}
}

func TestApplyConnectionsJWTUsesGatewayNamespace(t *testing.T) {
	cfg := settings.Settings{}
	conns := map[string]connectionDefinition{
		"gateway": {
			API:     "openai",
			BaseURL: "https://agent.example.com/api/v1/agent",
			Credential: &connectionCredentialDecl{
				Source: "jwt",
			},
			// no models → gateway-picks-model
		},
	}
	applyConnections(&cfg, conns)

	mc := cfg.Models["gateway"]
	if mc.Credential != (settings.CredentialRef{Namespace: "gateway", Name: "default"}) {
		t.Errorf("credential = %+v, want gateway/default", mc.Credential)
	}
	if cc, ok := cfg.Credentials["gateway"]["default"]; !ok || cc.Source != "injected" {
		t.Errorf("credentials[gateway][default] = %+v, want {injected}", cc)
	}
}

func TestApplyConnectionsRuntimeAliasIsFriendlyName(t *testing.T) {
	cfg := settings.Settings{}
	conns := map[string]connectionDefinition{
		"deepseek": {
			API:     "openai",
			BaseURL: "https://api.deepseek.com",
			Credential: &connectionCredentialDecl{
				Source: "injected",
				Ref:    "deepseek",
			},
			Models: []connectionModelDef{
				{
					WireModelID:  "deepseek-v4-flash",
					RuntimeAlias: "provider.ZGVlcHNlZWs.model.ZGVlcHNlZWstdjQtZmxhc2g",
				},
			},
		},
	}
	applyConnections(&cfg, conns)

	// The model must be keyed by the runtime alias (host's picker key), with
	// the wire model id preserved as the request body string.
	if _, ok := cfg.Models["provider.ZGVlcHNlZWs.model.ZGVlcHNlZWstdjQtZmxhc2g"]; !ok {
		t.Fatalf("model not keyed by runtime alias; keys=%v", keys(cfg.Models))
	}
	mc := cfg.Models["provider.ZGVlcHNlZWs.model.ZGVlcHNlZWstdjQtZmxhc2g"]
	if mc.Model != "deepseek-v4-flash" {
		t.Errorf("wire model = %q, want deepseek-v4-flash", mc.Model)
	}
	// The bare wire id must NOT be a key.
	if _, ok := cfg.Models["deepseek-v4-flash"]; ok {
		t.Error("model should not also be keyed by bare wire model id")
	}
}

func TestApplyConnectionsExplicitNamespaceOverridesSource(t *testing.T) {
	cfg := settings.Settings{}
	conns := map[string]connectionDefinition{
		"my-provider": {
			API:     "openai",
			BaseURL: "https://proxy.example.com/v1",
			Credential: &connectionCredentialDecl{
				Source:    "injected",
				Namespace: "llm",
				Ref:       "my-key",
			},
			Models: []connectionModelDef{
				{WireModelID: "model-a"},
			},
		},
	}
	applyConnections(&cfg, conns)

	mc := cfg.Models["model-a"]
	if mc.Credential != (settings.CredentialRef{Namespace: "llm", Name: "my-key"}) {
		t.Errorf("credential = %+v, want llm/my-key (explicit namespace)", mc.Credential)
	}
}

func TestApplyConnectionsGatewayAPIUsesOpenAIProvider(t *testing.T) {
	cfg := settings.Settings{}
	conns := map[string]connectionDefinition{
		"talkify-gateway": {
			API:     "gateway", // legacy connection-injection value
			BaseURL: "https://gateway.example.com/v1",
			Credential: &connectionCredentialDecl{
				Source: "injected",
				Ref:    "gateway",
			},
			Models: []connectionModelDef{
				{WireModelID: "deepseek-v4-flash"},
			},
		},
	}
	applyConnections(&cfg, conns)

	mc := cfg.Models["deepseek-v4-flash"]
	if mc.Provider != "openai" {
		t.Errorf("provider = %q, want openai", mc.Provider)
	}
	if mc.Credential != (settings.CredentialRef{Namespace: "gateway", Name: "default"}) {
		t.Errorf("credential = %+v, want gateway/default", mc.Credential)
	}
}

func keys(m map[string]settings.ModelConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
