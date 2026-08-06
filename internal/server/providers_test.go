package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/settings"
)

func testProviderMux(t *testing.T, store *ProviderStore) http.Handler {
	t.Helper()
	base := NewMux(nil, nil, nil, MuxOptions{Providers: store, ServerAuth: ServerAuth{Enabled: false}})
	return base
}

// PUT then GET round-trips a provider through the settings file; the response
// carries the grouped model array but strips nothing sensitive (no headers
// stored in this test).
func TestProviderStoreUpsertAndGet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".codeagent", "settings.json")
	store := NewProviderStore(path, nil)

	handler := testProviderMux(t, store)

	// PUT dashscope with two models.
	body := `{"base_url":"https://dashscope.aliyuncs.com/compatible-mode/v1","api":"openai","models":[{"id":"qwen3-coder-plus"},{"id":"qwen3.7-max"}]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/dashscope", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// GET the list.
	req = httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list status = %d", rec.Code)
	}
	var env struct {
		Data struct {
			Providers []ProviderDTO `json:"providers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	list := env.Data.Providers
	if len(list) != 1 || list[0].ID != "dashscope" {
		t.Fatalf("list = %+v, want one dashscope provider", list)
	}
	if len(list[0].Models) != 2 {
		t.Errorf("models = %+v, want 2", list[0].Models)
	}
	if list[0].BaseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Errorf("base_url = %q", list[0].BaseURL)
	}
}

// PUT with empty models is rejected.
func TestProviderStoreUpsertRejectsEmptyModels(t *testing.T) {
	store := NewProviderStore(filepath.Join(t.TempDir(), "s.json"), nil)
	handler := testProviderMux(t, store)
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/x", bytes.NewBufferString(`{"base_url":"http://x"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// GET of an unknown provider is 404.
func TestProviderStoreGetUnknown(t *testing.T) {
	store := NewProviderStore(filepath.Join(t.TempDir(), "s.json"), nil)
	handler := testProviderMux(t, store)
	req := httptest.NewRequest(http.MethodGet, "/v1/providers/nope", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// DELETE of a provider referenced by default_model is refused (409).
func TestProviderStoreDeleteInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	// Seed a settings file with default_model referencing provider "dashscope".
	seed := `{"default_model":"dashscope/qwen3-coder-plus","providers":{"dashscope":{"base_url":"http://x","models":[{"id":"qwen3-coder-plus"}]}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewProviderStore(path, nil)
	handler := testProviderMux(t, store)

	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/dashscope", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (default_model references it)", rec.Code)
	}
}

// DELETE of an unreferenced provider succeeds and persists.
func TestProviderStoreDeleteOK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	seed := `{"providers":{"orphan":{"base_url":"http://x","models":[{"id":"m"}]}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewProviderStore(path, nil)
	handler := testProviderMux(t, store)

	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/orphan", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	// Persisted: reload the settings file and confirm the provider is gone.
	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Providers["orphan"]; ok {
		t.Error("provider still present after delete")
	}
}

// Reconfigure failure rolls the disk back to the pre-Upsert state.
func TestProviderStoreUpsertRollbackOnReconfigureError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	seed := `{"providers":{"existing":{"base_url":"http://old","models":[{"id":"m"}]}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	failReconfigure := func() error { return errProviderApplyFailed }
	store := NewProviderStore(path, failReconfigure)

	body := `{"base_url":"http://new","models":[{"id":"m"},{"id":"m2"}]}`
	handler := testProviderMux(t, store)
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/existing", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (reconfigure failed)", rec.Code)
	}
	// Disk must show the OLD state.
	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Providers["existing"]; len(got.Models) != 1 || got.BaseURL != "http://old" {
		t.Errorf("rollback failed: provider = %+v, want 1 model + old base_url", got)
	}
}

var errProviderApplyFailed = &applyFailedError{}

type applyFailedError struct{}

func (e *applyFailedError) Error() string { return "reconfigure failed (test)" }

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
