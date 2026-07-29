package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"code-agent/internal/app"
)

func TestServerAuthProtectsEveryNonHealthRoute(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := withServerAuth(next, ServerAuth{
		Enabled: true, Token: token, PublicHealthz: true,
	})

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusNoContent {
		t.Fatalf("public health status = %d", health.Code)
	}

	for _, tc := range []struct {
		name, header, code string
	}{
		{name: "missing", code: "runtime_auth_required"},
		{name: "wrong", header: "Bearer wrong", code: "runtime_auth_invalid"},
		{name: "basic", header: "Basic dXNlcjpwYXNz", code: "runtime_auth_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/runtime/info", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var envelope struct {
				Code int              `json:"code"`
				Data runtimeAuthError `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Code != CodeUnauthorized || envelope.Data.Code != tc.code {
				t.Fatalf("auth envelope = %+v", envelope)
			}
		})
	}

	allowed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations/any/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(allowed, req)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("authorized status = %d", allowed.Code)
	}

	protectedHealth := httptest.NewRecorder()
	withServerAuth(next, ServerAuth{
		Enabled: true, Token: token, PublicHealthz: false,
	}).ServeHTTP(protectedHealth, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if protectedHealth.Code != http.StatusUnauthorized {
		t.Fatalf("protected health status = %d", protectedHealth.Code)
	}
}

func TestExternalDeploymentSecurity(t *testing.T) {
	t.Setenv("RUNTIME_TOKEN", "0123456789abcdef0123456789abcdef")
	auth, err := ResolveExternalServerAuth(app.ServerConfig{
		Authentication: "bearer", AccessTokenEnv: "RUNTIME_TOKEN", PublicHealthz: true,
	})
	if err != nil || !auth.Enabled {
		t.Fatalf("ResolveExternalServerAuth = %+v, %v", auth, err)
	}
	if err := ValidateExternalDeployment("127.0.0.1:8797", app.ServerConfig{}, ServerAuth{}); err != nil {
		t.Fatalf("loopback unauthenticated deployment rejected: %v", err)
	}
	if err := ValidateExternalDeployment(":8797", app.ServerConfig{}, ServerAuth{}); err == nil {
		t.Fatal("non-loopback unauthenticated deployment must fail")
	}
	if err := ValidateExternalDeployment("0.0.0.0:8797", app.ServerConfig{}, auth); err == nil {
		t.Fatal("non-loopback bearer deployment without TLS must fail")
	}
	tlsConfig := app.ServerConfig{TLSCertificate: "server.crt", TLSPrivateKey: "server.key"}
	if err := ValidateExternalDeployment("0.0.0.0:8797", tlsConfig, auth); err != nil {
		t.Fatalf("authenticated TLS deployment rejected: %v", err)
	}
}
