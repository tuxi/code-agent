package runtime

import (
	"code-agent/internal/settings"
	"context"
	"testing"
)

func TestManagedExecutorSelectionRequiresCanonicalAvailableModel(t *testing.T) {
	mc := settings.ModelConfig{Name: "fast", Provider: "openai", Model: "vendor/model", BaseURL: "http://127.0.0.1:12345", Catalog: settings.ModelCatalogMetadata{ConnectionID: "corp"}}
	b := &ServeRunBuilder{Set: settings.Settings{Models: map[string]settings.ModelConfig{"fast": mc}}, mc: mc}
	if err := b.ValidateExecutorModel(context.Background(), "corp/vendor/model"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "fast", "other/vendor/model"} {
		if err := b.ValidateExecutorModel(context.Background(), id); err == nil {
			t.Fatal("accepted noncatalog ID", id)
		}
	}
	mc.BaseURL = "https://example.invalid/v1"
	b.ReconfigureSettings(settings.Settings{Models: map[string]settings.ModelConfig{"fast": mc}}, mc, nil, nil)
	if err := b.ValidateExecutorModel(context.Background(), "corp/vendor/model"); err == nil {
		t.Fatal("missing credentials accepted")
	}
}

func TestNamespacedTurnSelectionAndDefaultIsolation(t *testing.T) {
	base := settings.ModelConfig{Name: "fast", Provider: "openai", Model: "vendor/model", BaseURL: "http://127.0.0.1:12345", Catalog: settings.ModelCatalogMetadata{ConnectionID: "corp"}}
	cfg := settings.Settings{DefaultModel: "fast", Models: map[string]settings.ModelConfig{"fast": base}}
	got, err := resolveTurnModel(cfg, base, "corp/vendor/model")
	if err != nil || got.Name != "fast" || got.BaseURL != base.BaseURL {
		t.Fatalf("selection=%+v %v", got, err)
	}
	for _, requested := range []string{"typo", "corp/missing", "other/vendor/model", "deepseek"} {
		if _, err := resolveTurnModel(cfg, base, requested); err == nil {
			t.Fatalf("unknown %q fell back", requested)
		}
	}
	got, err = resolveTurnModel(cfg, base, "")
	if err != nil || got.Name != base.Name || got.Model != base.Model {
		t.Fatalf("default was changed: %+v %v", got, err)
	}
	gateway := base
	gateway.BaseURL = "https://example.invalid/api/v1/agent"
	if _, err := resolveTurnModel(cfg, gateway, "corp/missing"); err == nil {
		t.Fatal("namespaced ID escaped strictness through gateway")
	}
}
