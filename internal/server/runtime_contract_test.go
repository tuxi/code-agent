package server

import (
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/app"
	runtimepkg "code-agent/internal/runtime"
)

func boolPointer(value bool) *bool { return &value }

func TestRuntimeContractPersistsIdentityAndCatalogRevisionWithoutSecrets(t *testing.T) {
	oldBase := runtimepkg.StoreBaseDir()
	runtimepkg.SetStoreBaseDir(t.TempDir())
	t.Cleanup(func() { runtimepkg.SetStoreBaseDir(oldBase) })

	alias := "deepseek/deepseek-chat"
	cfg := app.Config{
		DefaultModel: alias,
		Models: map[string]app.ModelConfig{
			alias: {
				Name: alias, Provider: "openai",
				BaseURL: "https://secret-provider.example/v1", Model: "deepseek-chat",
				ContextWindow: 128000,
				Credential: app.CredentialRef{Namespace: "llm", Name: "deepseek-prod"},
				Catalog: app.ModelCatalogMetadata{
					ConnectionID: "deepseek-prod", ProviderID: "deepseek",
					ConnectionDisplayName: "DeepSeek Production",
					DisplayName:           "DeepSeek Chat", SupportsTools: boolPointer(true),
					InputModalities: []string{"image", "text"},
				},
			},
		},
	}

	info1, catalog1, err := BuildRuntimeContract(cfg, "/workspace", "Xiaoyuan Mac", RuntimeProfileHeadless)
	if err != nil {
		t.Fatal(err)
	}
	info2, catalog2, err := BuildRuntimeContract(cfg, "/workspace", "Xiaoyuan Mac", RuntimeProfileHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if info1.ServerID == "" || info1.ServerID != info2.ServerID {
		t.Fatalf("server IDs = %q, %q", info1.ServerID, info2.ServerID)
	}
	if catalog1.Revision != 1 || catalog2.Revision != 1 {
		t.Fatalf("catalog revisions = %d, %d", catalog1.Revision, catalog2.Revision)
	}
	if info1.Schema != "runtime-info/v1" || info1.Product != "codeagent" ||
		info1.AgentWireProtocol.Major != 1 || info1.RuntimeProfile != RuntimeProfileHeadless {
		t.Fatalf("runtime info = %+v", info1)
	}
	if len(catalog1.Connections) != 1 || len(catalog1.Connections[0].Models) != 1 {
		t.Fatalf("catalog = %+v", catalog1)
	}
	model := catalog1.Connections[0].Models[0]
	if model.RuntimeAlias != alias || model.WireModelID != "deepseek-chat" ||
		!model.SupportsTools || len(model.InputModalities) != 2 {
		t.Fatalf("catalog model = %+v", model)
	}
	// R3.1: availability is real. The test config has no credential for the
	// model, so it must be listed-but-unavailable with a reason (not the old
	// hardcoded true).
	if model.Available {
		t.Errorf("model should be unavailable without a credential, got %+v", model)
	}
	if model.UnavailableReason != "no_auth" {
		t.Errorf("UnavailableReason = %q, want no_auth", model.UnavailableReason)
	}
	encoded, _ := json.Marshal(catalog1)
	for _, forbidden := range []string{
		"secret-provider.example",
		"secret-api-key",
		`"base_url"`,
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("catalog leaked %q: %s", forbidden, encoded)
		}
	}
	// R3.1: the credential subobject is non-secret (status/source only) — it
	// must never carry a secret value (N1.2).
	if cred := catalog1.Connections[0].Credential; cred == nil {
		t.Error("expected connection credential subobject")
	} else if cred.Status != "missing" || cred.Source != "env" {
		t.Errorf("credential = %+v, want {missing env}", cred)
	}

	changed := cfg
	changed.Models = make(map[string]app.ModelConfig, len(cfg.Models))
	for name, modelConfig := range cfg.Models {
		modelConfig.Catalog.DisplayName = "DeepSeek Chat Updated"
		changed.Models[name] = modelConfig
	}
	info3, catalog3, err := BuildRuntimeContract(changed, "/workspace", "Xiaoyuan Mac", RuntimeProfileHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if info3.ServerID != info1.ServerID || catalog3.Revision != 2 {
		t.Fatalf("changed contract = server %q revision %d", info3.ServerID, catalog3.Revision)
	}
}

func TestRuntimeModelCatalogAllowsZeroModels(t *testing.T) {
	oldBase := runtimepkg.StoreBaseDir()
	runtimepkg.SetStoreBaseDir(t.TempDir())
	t.Cleanup(func() { runtimepkg.SetStoreBaseDir(oldBase) })
	_, catalog, err := BuildRuntimeContract(
		app.Config{Models: map[string]app.ModelConfig{}}, "/workspace", "", RuntimeProfileSandboxed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Schema != "runtime-model-catalog/v2" || catalog.DefaultRuntimeAlias != "" ||
		catalog.Connections == nil || len(catalog.Connections) != 0 {
		t.Fatalf("zero-model catalog = %+v", catalog)
	}
}
