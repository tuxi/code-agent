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

func TestProviderStoreNormalizesLegacyGatewayAPI(t *testing.T) {
	store := NewProviderStore(filepath.Join(t.TempDir(), "s.json"), nil)
	handler := testProviderMux(t, store)
	body := `{"base_url":"https://gateway.example.com/api/v1/agent","api":"gateway","credential":{"namespace":"gateway","name":"default"},"models":[{"id":"deepseek-v4-flash"}]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/talkify-gateway", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, err := store.Get("talkify-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if got.API != "openai" {
		t.Errorf("api = %q, want openai", got.API)
	}
	if got.Credential != (ProviderCred{Namespace: "gateway", Name: "default"}) {
		t.Errorf("credential = %+v, want gateway/default", got.Credential)
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
	// OQ2: DELETE now returns 200 with an applied marker (no more 204).
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Data struct {
			Applied bool `json:"applied"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Applied {
		t.Error("applied = true, want false (reconfigure is nil → restart required)")
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

// PUT of a known built-in api_key provider (opencode-go) auto-declares the
// matching credentials entry {source:"env", env:OPENCODE_GO_API_KEY} so the
// runtime resolver chain can resolve it without manual settings edits.
func TestProviderStoreUpsertAutoDeclaresCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	store := NewProviderStore(path, nil)
	handler := testProviderMux(t, store)

	body := `{"base_url":"https://opencode.ai/zen/go/v1","api":"openai","models":[{"id":"deepseek-v4-flash"}]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/opencode-go", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := f.Credentials["llm"]["opencode-go"]
	if !ok {
		t.Fatalf("credentials.llm.opencode-go missing; credentials=%+v", f.Credentials)
	}
	if cc.Source != "env" || cc.Env != "OPENCODE_GO_API_KEY" {
		t.Errorf("credential = %+v, want {source:env, env:OPENCODE_GO_API_KEY}", cc)
	}
}

// PUT never overwrites a pre-existing credentials entry: the user may have
// pointed it at a keychain/injected source.
func TestProviderStoreUpsertKeepsExistingCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	seed := `{"credentials":{"llm":{"opencode-go":{"source":"keychain"}}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewProviderStore(path, nil)
	handler := testProviderMux(t, store)

	body := `{"base_url":"https://opencode.ai/zen/go/v1","api":"openai","models":[{"id":"deepseek-v4-flash"}]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/opencode-go", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	cc := f.Credentials["llm"]["opencode-go"]
	if cc.Source != "keychain" {
		t.Errorf("credential source = %q, want pre-existing keychain preserved", cc.Source)
	}
}

// Reconfigure failure rolls back the auto-declared credential along with the
// provider, leaving no residue in the credentials section.
func TestProviderStoreUpsertRollbackRemovesCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	store := NewProviderStore(path, func() error { return errProviderApplyFailed })
	handler := testProviderMux(t, store)

	body := `{"base_url":"https://opencode.ai/zen/go/v1","api":"openai","models":[{"id":"deepseek-v4-flash"}]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/opencode-go", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (reconfigure failed)", rec.Code)
	}

	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Providers["opencode-go"]; ok {
		t.Error("provider present after rollback")
	}
	if _, ok := f.Credentials["llm"]["opencode-go"]; ok {
		t.Errorf("auto-declared credential not rolled back; credentials=%+v", f.Credentials)
	}
}

// Custom (non-registry) providers get no auto-declared credential: their env
// var name is unknown, so the user configures credentials manually.
func TestProviderStoreUpsertCustomProviderNoCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	store := NewProviderStore(path, nil)
	handler := testProviderMux(t, store)

	body := `{"base_url":"http://x","api":"openai","models":[{"id":"m"}]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/custom-svc", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Credentials) != 0 {
		t.Errorf("credentials = %+v, want empty for custom provider", f.Credentials)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
