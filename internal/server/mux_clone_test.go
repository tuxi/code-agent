package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newCloneMux(workspaceRoot string) http.Handler {
	return NewMux(newFakeConversationRepo(), &fakeEventStore{}, nil, MuxOptions{WorkspaceRoot: workspaceRoot})
}

func TestClone_NoWorkspaceRoot(t *testing.T) {
	srv := httptest.NewServer(newCloneMux("")) // disabled
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
	srv := httptest.NewServer(newCloneMux(t.TempDir()))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/repos/clone", "application/json",
		strings.NewReader(`{"url":"https://gitlab.com/o/r"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body cloneErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error != "invalid_url" {
		t.Errorf("error code = %q, want invalid_url", body.Error)
	}
}

func TestClone_InvalidName_HTTP(t *testing.T) {
	srv := httptest.NewServer(newCloneMux(t.TempDir()))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/repos/clone", "application/json",
		strings.NewReader(`{"url":"owner/repo","name":"../escape"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body cloneErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error != "invalid_name" {
		t.Errorf("error code = %q, want invalid_name", body.Error)
	}
}

func TestClone_BadJSON(t *testing.T) {
	srv := httptest.NewServer(newCloneMux(t.TempDir()))
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

func TestStatusForCloneCode(t *testing.T) {
	cases := map[string]int{
		"invalid_url":    http.StatusBadRequest,
		"invalid_name":   http.StatusBadRequest,
		"repo_not_found": http.StatusNotFound,
		"ref_not_found":  http.StatusNotFound,
		"network_error":  http.StatusBadGateway,
		"io_error":       http.StatusInternalServerError,
		"something_else": http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := statusForCloneCode(code); got != want {
			t.Errorf("statusForCloneCode(%q) = %d, want %d", code, got, want)
		}
	}
}
