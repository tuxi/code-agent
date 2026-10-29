package server

import (
	"bytes"
	"code-agent/internal/model"
	"code-agent/internal/settings"
	"fmt"
	"testing"
	"time"
)

func TestModelDiscoverySeparatesConfigurationAvailabilityVerification(t *testing.T) {
	connection := fmt.Sprintf("discovery-%d", time.Now().UnixNano())
	mc := settings.ModelConfig{Name: connection + "/model", Provider: "openai", Model: "model", BaseURL: "http://127.0.0.1:12345", Catalog: settings.ModelCatalogMetadata{ConnectionID: connection}}
	cfg := settings.Settings{DefaultModel: mc.Name, Models: map[string]settings.ModelConfig{mc.Name: mc}}
	get := func() RuntimeModelDescriptor {
		c := BuildRuntimeModelCatalog(cfg, nil)
		if c.DefaultModel != connection+"/model" || c.DefaultRuntimeAlias != mc.Name || c.VerificationScope != "process" {
			t.Fatalf("catalog=%+v", c)
		}
		return c.Connections[0].Models[0]
	}
	m := get()
	if !m.Configured || !m.Available || m.Verified {
		t.Fatalf("unverified config=%+v", m)
	}
	model.RecordVerification(model.Selection{ConfigurationID: m.ConfigurationID, ActualModel: "actual-model-revision"}, time.Now())
	m = get()
	if !m.Verified || m.VerifiedAt == nil || m.ActualModel != "actual-model-revision" {
		t.Fatalf("verified=%+v", m)
	}
	mc.BaseURL = "http://127.0.0.1:12346"
	cfg.Models[mc.Name] = mc
	if get().Verified {
		t.Fatal("changed route retained verification")
	}
	model.RecordVerification(model.Selection{ConfigurationID: settings.ModelConfigurationID(mc)}, time.Now().Add(-25*time.Hour))
	if get().Verified {
		t.Fatal("stale success presented as recent verification")
	}
}

func TestCatalogOpaqueDefaultAndVerificationDoNotChangeConfiguration(t *testing.T) {
	mc := settings.ModelConfig{Name: "friendly", Provider: "openai", Model: "model", BaseURL: "http://127.0.0.1:12345", Catalog: settings.ModelCatalogMetadata{ConnectionID: "corp"}}
	cfg := settings.Settings{DefaultModel: "provider.Y29ycA.model.bW9kZWw", Models: map[string]settings.ModelConfig{"friendly": mc, "provider.Y29ycA.model.bW9kZWw": mc}}
	catalog := BuildRuntimeModelCatalog(cfg, nil)
	if catalog.DefaultModel != "corp/model" || catalog.DefaultRuntimeAlias != "friendly" {
		t.Fatalf("opaque default=%+v", catalog)
	}
	before, err := catalogConfigurationFingerprint(catalog)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	catalog.Connections[0].Models[0].Verified = true
	catalog.Connections[0].Models[0].VerifiedAt = &now
	after, err := catalogConfigurationFingerprint(catalog)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("observation changed configuration fingerprint")
	}
	if !catalog.Connections[0].Models[0].Verified {
		t.Fatal("fingerprint mutated live catalog")
	}
}

func TestCatalogReportsRuntimeOverrideInsteadOfSettingsDefault(t *testing.T) {
	cfg := settings.Settings{DefaultModel: "first/model", Models: map[string]settings.ModelConfig{
		"first/model":  {Name: "first/model", Model: "model", Provider: "openai", BaseURL: "http://127.0.0.1:1", Catalog: settings.ModelCatalogMetadata{ConnectionID: "first"}},
		"second/model": {Name: "second/model", Model: "model", Provider: "openai", BaseURL: "http://127.0.0.1:2", Catalog: settings.ModelCatalogMetadata{ConnectionID: "second"}},
	}}
	catalog := BuildRuntimeModelCatalog(cfg, nil)
	mc := cfg.Models["second/model"]
	updated := catalogWithRuntimeDefault(catalog, &mc)
	if updated.DefaultModel != "second/model" || catalog.DefaultModel != "first/model" {
		t.Fatalf("default=%+v", updated)
	}
	if cleared := catalogWithRuntimeDefault(catalog, nil); cleared.DefaultModel != "" {
		t.Fatal("no-model runtime retained stale default")
	}
}
