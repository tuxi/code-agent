package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"code-agent/internal/automation"
)

// newAutomationTestMux builds a NewMux with a real automation store in a temp dir.
func newAutomationTestMux(t *testing.T) http.Handler {
	t.Helper()
	store, err := automation.OpenStore(filepath.Join(t.TempDir(), "automations.db"))
	if err != nil {
		t.Fatalf("open automation store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return NewMux(nil, nil, nil, MuxOptions{AutomationStore: store})
}

func TestAutomationRoutesCRUD(t *testing.T) {
	h := newAutomationTestMux(t)

	// POST create
	createBody := `{"name":"daily","prompt":"summarize","schedule_type":"recurring","rrule":"FREQ=DAILY;BYHOUR=9","timezone":"UTC"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/automations", bytes.NewReader([]byte(createBody))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created automationDTO
	if err := decodeData(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Name != "daily" {
		t.Fatalf("created = %+v", created)
	}

	// GET list
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/automations", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list []automationDTO
	if err := decodeData(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	// GET detail
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/automations/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d", rec.Code)
	}

	// PATCH pause
	patchBody := `{"enabled":false}`
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/automations/"+created.ID, bytes.NewReader([]byte(patchBody))))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var patched automationDTO
	if err := decodeData(rec.Body.Bytes(), &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Status != "PAUSED" {
		t.Fatalf("patched status = %q, want PAUSED", patched.Status)
	}

	// DELETE
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/automations/"+created.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}

	// GET detail after delete -> 404
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/automations/"+created.ID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("detail after delete status = %d, want 404", rec.Code)
	}
}

func TestAutomationRoutesValidation(t *testing.T) {
	h := newAutomationTestMux(t)
	// once without scheduled_at -> 400
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/automations",
		bytes.NewReader([]byte(`{"name":"x","prompt":"p","schedule_type":"once","timezone":"UTC"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("once-no-scheduled_at status = %d, want 400", rec.Code)
	}
	// recurring without rrule -> 400
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/automations",
		bytes.NewReader([]byte(`{"name":"x","prompt":"p","schedule_type":"recurring","timezone":"UTC"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("recurring-no-rrule status = %d, want 400", rec.Code)
	}
}

func TestAutomationRoutesRuns(t *testing.T) {
	h := newAutomationTestMux(t)
	// create
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/automations",
		bytes.NewReader([]byte(`{"name":"a","prompt":"p","schedule_type":"recurring","rrule":"FREQ=DAILY","timezone":"UTC"}`))))
	var created automationDTO
	_ = decodeData(rec.Body.Bytes(), &created)

	// runs endpoint returns empty list (no runs yet)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/automations/"+created.ID+"/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("runs status = %d", rec.Code)
	}
	var runs []automation.Run
	if err := decodeData(rec.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected empty runs, got %d", len(runs))
	}
}

func TestAutomationRoutesDisabledWhenNilStore(t *testing.T) {
	h := NewMux(nil, nil, nil, MuxOptions{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/automations", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil store status = %d, want 404", rec.Code)
	}
}

// decodeData unwraps the writeJSON envelope {trace_id, code, msg, data} and
// decodes the data payload into v.
func decodeData(body []byte, v any) error {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return err
	}
	return json.Unmarshal(env.Data, v)
}
