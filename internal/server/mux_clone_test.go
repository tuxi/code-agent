package server

import (
	"code-agent/internal/repos"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newCloneMux(t *testing.T, workspaceRoot string) http.Handler {
	t.Helper()
	service, err := repos.NewService(workspaceRoot, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return NewMux(newFakeConversationRepo(), &fakeEventStore{}, nil, MuxOptions{CloneService: service})
}

func TestClone_NoWorkspaceRoot(t *testing.T) {
	srv := httptest.NewServer(NewMux(newFakeConversationRepo(), &fakeEventStore{}, nil, MuxOptions{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/repos/clone", "application/json",
		strings.NewReader(`{"url":"owner/repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestClone_InvalidURL_HTTP(t *testing.T) {
	srv := httptest.NewServer(newCloneMux(t, t.TempDir()))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/repos/clone", "application/json",
		strings.NewReader(`{"request_id":"req-1","url":"ssh://git@gitlab.com/o/r"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body cloneErrorResponse
	decodeResponse(t, resp, &body)
	if body.Error != "invalid_url" {
		t.Errorf("error code = %q, want invalid_url", body.Error)
	}
}

func TestClone_InvalidName_HTTP(t *testing.T) {
	srv := httptest.NewServer(newCloneMux(t, t.TempDir()))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/repos/clone", "application/json",
		strings.NewReader(`{"request_id":"req-1","url":"owner/repo","name":"../escape"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body cloneErrorResponse
	decodeResponse(t, resp, &body)
	if body.Error != "invalid_name" {
		t.Errorf("error code = %q, want invalid_name", body.Error)
	}
}

func TestClone_BadJSON(t *testing.T) {
	srv := httptest.NewServer(newCloneMux(t, t.TempDir()))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/repos/clone", "application/json",
		strings.NewReader(`{not json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestClone_RequestIDRequired(t *testing.T) {
	h := newCloneMux(t, t.TempDir())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/repos/clone", strings.NewReader(`{"url":"owner/repo"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	var body cloneErrorResponse
	decodeResponse(t, rec.Result(), &body)
	if body.Error != "invalid_request" {
		t.Fatalf("error=%q, want invalid_request", body.Error)
	}
}

func TestCloneCapabilityReturnsProjectsRoot(t *testing.T) {
	root := t.TempDir()
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	h := newCloneMux(t, root)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runtime/capabilities", nil))
	var body runtimeCapabilitiesResponse
	decodeResponse(t, rec.Result(), &body)
	if !body.Capabilities.PublicGitClone || body.ProjectsRoot != wantRoot {
		t.Fatalf("capabilities=%+v projects_root=%q", body.Capabilities, body.ProjectsRoot)
	}
}

func TestStatusForCloneCode(t *testing.T) {
	cases := map[string]int{
		"invalid_url":          http.StatusBadRequest,
		"invalid_name":         http.StatusBadRequest,
		"invalid_request":      http.StatusBadRequest,
		"repo_not_found":       http.StatusNotFound,
		"ref_not_found":        http.StatusNotFound,
		"network_error":        http.StatusBadGateway,
		"io_error":             http.StatusInternalServerError,
		"destination_conflict": http.StatusConflict,
		"cancelled":            http.StatusRequestTimeout,
		"something_else":       http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := statusForCloneCode(code); got != want {
			t.Errorf("statusForCloneCode(%q) = %d, want %d", code, got, want)
		}
	}
}
