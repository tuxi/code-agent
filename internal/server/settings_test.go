package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSettingsReloadRoute(t *testing.T) {
	called := 0
	h := NewMux(nil, nil, nil, MuxOptions{
		ReloadSettings: func() error {
			called++
			return nil
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/settings/reload", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if called != 1 {
		t.Fatalf("reload callback called %d times, want 1", called)
	}
}

func TestSettingsReloadRouteReportsApplyError(t *testing.T) {
	h := NewMux(nil, nil, nil, MuxOptions{
		ReloadSettings: func() error { return errTestSettingsReload },
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/settings/reload", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

var errTestSettingsReload = testSettingsReloadError{}

type testSettingsReloadError struct{}

func (testSettingsReloadError) Error() string { return "settings reload failed" }
