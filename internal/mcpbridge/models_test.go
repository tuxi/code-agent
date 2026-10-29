package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCatalogSelectionRejectsUnavailableWithoutSubmission(t *testing.T) {
	c, f := setup(t)
	available := false
	var catalogReads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/models" {
			f.serve(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("catalog request lost authentication")
		}
		catalogReads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"default_model": "corp/vendor/model", "default_runtime_alias": "friendly", "connections": []any{
			map[string]any{"id": "corp", "provider_id": "custom", "api_key": "must-not-escape", "models": []any{
				map[string]any{"id": "corp/vendor/model", "runtime_alias": "friendly", "wire_model_id": "vendor/model", "configured": true, "available": available, "unavailable_reason": "no_auth"},
			}},
		}})
	}))
	defer srv.Close()
	c.base, _ = url.Parse(srv.URL)
	ctx := context.Background()
	catalog, err := c.ListModels(ctx)
	if err != nil || catalog.DefaultModel != "corp/vendor/model" || len(catalog.Models) != 1 {
		t.Fatalf("catalog=%+v %v", catalog, err)
	}
	data, _ := json.Marshal(catalog)
	if strings.Contains(string(data), "must-not-escape") {
		t.Fatal("catalog leaked private config")
	}
	ref, err := c.Create(ctx, "create")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"corp/vendor/model", "unknown/model"} {
		if _, err := c.Send(ctx, ref.ID, "select", "work", id); err == nil {
			t.Fatal("unavailable/unknown selection admitted")
		}
	}
	f.mu.Lock()
	inputs := f.inputs
	f.mu.Unlock()
	if inputs != 0 || len(c.submissions) != 0 {
		t.Fatal("rejected selection started work or poisoned retry state")
	}
	// Omitted model preserves default semantics without requiring a catalog call.
	reads := catalogReads.Load()
	if _, err := c.Send(ctx, ref.ID, "default", "work", ""); err != nil {
		t.Fatal(err)
	}
	if catalogReads.Load() != reads {
		t.Fatal("default selection unexpectedly resolved in bridge")
	}
}

func TestLegacyCatalogUsesDeclaredAlias(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entry := map[string]any{"runtime_alias": "friendly", "wire_model_id": "vendor/model", "configured": true, "available": true}
			if !legacy {
				entry["id"] = "corp/vendor/model"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"default_runtime_alias": "friendly", "connections": []any{map[string]any{"id": "corp", "models": []any{entry}}}})
		}))
		u, _ := url.Parse(srv.URL)
		c := &Client{base: u, token: "test-token", http: srv.Client()}
		selected, err := c.resolveModel(context.Background(), "corp/vendor/model")
		srv.Close()
		want := "corp/vendor/model"
		if legacy {
			want = "friendly"
		}
		if err != nil || selected != want {
			t.Fatalf("legacy=%v selection=%q %v", legacy, selected, err)
		}
	}
}
