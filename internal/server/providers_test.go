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

// Reasoning capability fields round-trip through PUT → settings file → GET,
// so the client's Add Model sheet can persist the reasoning toggle, default
// effort, and supported effort levels for custom providers.
func TestProviderStoreRoundTripsReasoningFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	store := NewProviderStore(path, nil)
	handler := testProviderMux(t, store)

	body := `{"base_url":"https://api.custom.example/v1","api":"openai","models":[{"id":"reasoner-x","reasoning_effort":"high","supported_reasoning_efforts":["low","medium","high","x-high","max"],"can_disable_reasoning":false},{"id":"thinker-y","supports_reasoning":true}]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/custom-svc", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, err := store.Get("custom-svc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("models = %+v, want 2", got.Models)
	}
	m := got.Models[0]
	if m.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q, want high", m.ReasoningEffort)
	}
	if len(m.SupportedReasoningEfforts) != 5 || m.SupportedReasoningEfforts[3] != "x-high" {
		t.Errorf("supported_reasoning_efforts = %v, want low..max incl. x-high", m.SupportedReasoningEfforts)
	}
	if m.CanDisableReasoning == nil || *m.CanDisableReasoning {
		t.Errorf("can_disable_reasoning = %v, want false", m.CanDisableReasoning)
	}
	if got.Models[1].SupportsReasoning == nil || !*got.Models[1].SupportsReasoning {
		t.Errorf("supports_reasoning = %v, want true", got.Models[1].SupportsReasoning)
	}
	// Persisted on disk in the settings file (the canonical source).
	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	pm := f.Providers["custom-svc"].Models[0]
	if pm.ReasoningEffort != "high" || len(pm.SupportedReasoningEfforts) != 5 {
		t.Errorf("persisted model = %+v, want reasoning fields intact", pm)
	}
}

// GET /v1/provider-templates carries the official reasoning capability of each
// suggested model so the Add Model sheet can pre-fill the reasoning toggle,
// default effort, and supported effort levels.
func TestProviderTemplatesExposeReasoningCapability(t *testing.T) {
	templates := buildProviderTemplates()
	byID := make(map[string]ProviderTemplateDTO, len(templates))
	for _, tpl := range templates {
		byID[tpl.ID] = tpl
	}

	deepseek := byID["deepseek"]
	if len(deepseek.Models) == 0 {
		t.Fatal("deepseek template has no models")
	}
	flash := deepseek.Models[0]
	if !flash.SupportsReasoning {
		t.Errorf("deepseek-v4-flash supports_reasoning = false, want true")
	}
	if len(flash.SupportedReasoningEfforts) != 3 {
		t.Errorf("supported_reasoning_efforts = %v, want [low medium high]", flash.SupportedReasoningEfforts)
	}
	if flash.CanDisableReasoning == nil || !*flash.CanDisableReasoning {
		t.Errorf("can_disable_reasoning = %v, want true", flash.CanDisableReasoning)
	}

	// A model with a toggle but no standardized effort control (qwen3-coder)
	// exposes an empty efforts list — the host shows the toggle only.
	qwen := byID["qwen"]
	if len(qwen.Models) == 0 || !qwen.Models[0].SupportsReasoning {
		t.Fatalf("qwen template = %+v, want a reasoning-capable model", qwen.Models)
	}
	if len(qwen.Models[0].SupportedReasoningEfforts) != 0 {
		t.Errorf("qwen supported_reasoning_efforts = %v, want empty (toggle only)", qwen.Models[0].SupportedReasoningEfforts)
	}
}

// API-key-only onboarding: PUT of a built-in connection WITHOUT a models list
// persists an EMPTY (snapshot-free) models section — FromSettings merges the
// registry's models at expansion, so provider-side model releases reach
// existing settings.json files with zero changes. The GET view surfaces the
// registry ids. ollama (no suggested models) and custom ids still require an
// explicit list.
func TestProviderStoreUpsertBuiltinFillsModelsFromRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	store := NewProviderStore(path, nil)
	handler := testProviderMux(t, store)

	// deepseek: no models in the body.
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/deepseek", bytes.NewBufferString(`{"credential":{"namespace":"llm","name":"deepseek"}}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Persisted section stays snapshot-free: no model ids on disk.
	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Providers["deepseek"].Models; len(got) != 0 {
		t.Fatalf("persisted models = %+v, want empty (registry merges at expansion)", got)
	}
	// The GET view surfaces the registry ids so clients see the effective list.
	got, err := store.Get("deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 3 || got.Models[0].ID != "deepseek-v4-flash" {
		t.Fatalf("GET view models = %+v, want 3 registry ids", got.Models)
	}

	// ollama has no suggested models → explicit list still required.
	req = httptest.NewRequest(http.MethodPut, "/v1/providers/ollama", bytes.NewBufferString(`{}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PUT ollama without models status = %d, want 400", rec.Code)
	}

	// Custom (unknown) ids keep the explicit-models requirement.
	req = httptest.NewRequest(http.MethodPut, "/v1/providers/custom-svc", bytes.NewBufferString(`{"base_url":"http://x"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PUT custom without models status = %d, want 400", rec.Code)
	}

	// An explicit models list always wins over the registry merge.
	store2 := NewProviderStore(filepath.Join(t.TempDir(), "s.json"), nil)
	handler2 := testProviderMux(t, store2)
	req = httptest.NewRequest(http.MethodPut, "/v1/providers/deepseek", bytes.NewBufferString(`{"models":[{"id":"deepseek-v4-pro"}]}`))
	rec = httptest.NewRecorder()
	handler2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with explicit models status = %d; body=%s", rec.Code, rec.Body.String())
	}
	got2, err := store2.Get("deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Models) != 1 || got2.Models[0].ID != "deepseek-v4-pro" {
		t.Errorf("explicit models = %+v, want only deepseek-v4-pro", got2.Models)
	}
}

// DELETE of a built-in provider removes its auto-declared credential entry
// (credentials.llm.glm → {env:GLM_API_KEY}) from the file when it is no
// longer referenced by any other provider — disconnecting leaves no residue.
func TestProviderStoreDeleteRemovesOrphanedCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	store := NewProviderStore(path, nil)

	// Seed: glm provider with its auto-declared credential.
	root := `{"providers":{"glm":{"base_url":"https://open.bigmodel.cn/api/paas/v4","api":"openai","credentials":{"namespace":"llm","name":"glm"},"models":[{"id":"glm-5.3"}]}},"credentials":{"llm":{"glm":{"source":"env","env":"GLM_API_KEY"}}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := testProviderMux(t, store)

	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/glm", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Providers["glm"]; ok {
		t.Error("provider glm still present after DELETE")
	}
	if _, ok := f.Credentials["llm"]["glm"]; ok {
		t.Errorf("credentials.llm.glm still present, want removed (orphaned)")
	}
}

// DELETE keeps the credential entry when another provider shares the same ref.
func TestProviderStoreDeleteKeepsSharedCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	store := NewProviderStore(path, nil)

	// glm and sharer both reference llm/glm — deleting glm must not orphan the
	// credential because sharer still uses it.
	root := `{"providers":{"glm":{"base_url":"https://open.bigmodel.cn/api/paas/v4","api":"openai","credential":{"namespace":"llm","name":"glm"},"models":[{"id":"glm-5.3"}]},"sharer":{"base_url":"https://example.com/v1","api":"openai","credential":{"namespace":"llm","name":"glm"},"models":[{"id":"shared-model"}]}},"credentials":{"llm":{"glm":{"source":"env","env":"GLM_API_KEY"}}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := testProviderMux(t, store)

	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/glm", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Providers["glm"]; ok {
		t.Error("provider glm still present after DELETE")
	}
	if _, ok := f.Credentials["llm"]["glm"]; !ok {
		t.Error("credentials.llm.glm removed, want kept (still referenced by sharer)")
	}
}

// DELETE rolls the removed credential back when reconfigure fails, so disk
// never diverges from the runtime state.
func TestProviderStoreDeleteRollbackRestoresCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	store := NewProviderStore(path, func() error { return errProviderApplyFailed })

	root := `{"providers":{"glm":{"base_url":"https://open.bigmodel.cn/api/paas/v4","api":"openai","credential":{"namespace":"llm","name":"glm"},"models":[{"id":"glm-5.3"}]}},"credentials":{"llm":{"glm":{"source":"env","env":"GLM_API_KEY"}}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := testProviderMux(t, store)

	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/glm", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE status = %d, want 400 (reconfigure failed)", rec.Code)
	}
	// Disk must show the OLD state: provider AND credential restored.
	f, err := settings.ParseJSON(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Providers["glm"]; !ok {
		t.Error("provider glm not restored after rollback")
	}
	cc, ok := f.Credentials["llm"]["glm"]
	if !ok {
		t.Fatal("credentials.llm.glm not restored after rollback")
	}
	if cc.Source != "env" || cc.Env != "GLM_API_KEY" {
		t.Errorf("restored credential = %+v, want {env GLM_API_KEY}", cc)
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
