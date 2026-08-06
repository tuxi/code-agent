package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"code-agent/internal/app"
	"code-agent/internal/credential"
)

// A2: POST /v1/secrets updates the mutable injected resolver, and the next
// GET /v1/runtime/models rebuilds the catalog so the model becomes available.
func TestSecretsInjectionMakesModelAvailable(t *testing.T) {
	cfg, err := app.LoadConfigBytes([]byte(`
models:
  qwen:
    provider: openai
    base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
    model: qwen3-coder-plus
    credential: {namespace: llm, name: qwen}
`))
	if err != nil {
		t.Fatal(err)
	}
	// Startup catalog: qwen unavailable (no credential).
	startup := buildRuntimeModelCatalog(cfg, nil)
	if startupHasAvailable(startup, "qwen") {
		t.Fatal("startup catalog should have qwen unavailable (no injected credential)")
	}

	// Wire the mutable resolver + live builder.
	mut := credential.NewMutableResolver()
	builder := func() RuntimeModelCatalog { return buildRuntimeModelCatalog(cfg, mut) }
	opts := MuxOptions{
		RuntimeModelsBuilder: builder,
		InjectSecrets: func(targets map[credential.Target]credential.Credential) error {
			mut.SetAll(targets)
			return nil
		},
		ServerAuth: ServerAuth{Enabled: false},
	}
	mux := NewMux(nil, nil, nil, opts)

	// POST /v1/secrets with the qwen key.
	body := []byte(`{"llm/qwen": {"type": "bearer", "secret": "sk-qwen"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets", bytes.NewBuffer(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/secrets status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// GET /v1/runtime/models — live rebuild makes qwen available.
	req = httptest.NewRequest(http.MethodGet, "/v1/runtime/models", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET models status = %d", rec.Code)
	}
	var env struct {
		Data RuntimeModelCatalog `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if !catalogHasAvailable(env.Data, "qwen") {
		t.Errorf("qwen should be available after secrets injection: %s", rec.Body.String())
	}
}

// A2: nil InjectSecrets disables the endpoint (404).
func TestSecretsEndpointDisabledWhenNil(t *testing.T) {
	mux := NewMux(nil, nil, nil, MuxOptions{ServerAuth: ServerAuth{Enabled: false}})
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets", bytes.NewBufferString(`{"llm/qwen":"sk"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when InjectSecrets is nil", rec.Code)
	}
}

func startupHasAvailable(c RuntimeModelCatalog, connID string) bool {
	for _, c := range c.Connections {
		if c.ID == connID {
			for _, m := range c.Models {
				if m.Available {
					return true
				}
			}
		}
	}
	return false
}

func catalogHasAvailable(c RuntimeModelCatalog, connID string) bool {
	return startupHasAvailable(c, connID)
}
