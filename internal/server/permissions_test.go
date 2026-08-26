package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/conversation"
	"code-agent/internal/settings"
)

// fakePermissionService is an in-memory PermissionService for route tests.
type fakePermissionService struct {
	mode string
	set  string
}

func (f *fakePermissionService) EffectiveMode(root string) (string, error) {
	return f.mode, nil
}

func (f *fakePermissionService) SetMode(root, mode string) error {
	f.set = mode
	return nil
}

func TestPermissionsRoutesDisabledWhenNil(t *testing.T) {
	h := NewMux(newFakeConversationRepo(), &fakeEventStore{}, nil, MuxOptions{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/workspaces/permissions/foo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil Permissions: GET status = %d, want 404", resp.StatusCode)
	}
}

func TestPermissionsGetAndPut(t *testing.T) {
	svc := &fakePermissionService{mode: "auto"}
	h := NewMux(newFakeConversationRepo(), &fakeEventStore{}, nil, MuxOptions{Permissions: svc})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// GET returns the effective mode and the available tiers.
	resp, err := http.Get(srv.URL + "/v1/workspaces/permissions/foo")
	if err != nil {
		t.Fatal(err)
	}
	var got permissionResponse
	decodeResponse(t, resp, &got)
	if got.Scope != "workspace" || got.Path != "/foo" || got.Mode != "auto" {
		t.Errorf("GET = %+v, want scope=workspace path=/foo mode=auto", got)
	}
	if len(got.Available) != 3 || got.Available[0] != "ask" || got.Available[1] != "auto" || got.Available[2] != "full" {
		t.Errorf("available = %v, want [ask auto full]", got.Available)
	}

	// PUT with a valid mode.
	body := bytes.NewBufferString(`{"mode":"full"}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/workspaces/permissions/foo", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.set != "full" {
		t.Errorf("SetMode called with %q, want full", svc.set)
	}
	// The PUT response reuses the GET shape verbatim (scope/path/available/mode
	// + applied=true) so the client decodes both endpoints with one model.
	var putResp permissionResponse
	decodeResponse(t, rec.Result(), &putResp)
	if putResp.Scope != "workspace" || putResp.Path != "/foo" || putResp.Mode != "full" || !putResp.Applied {
		t.Errorf("PUT response = %+v, want scope=workspace path=/foo mode=full applied=true", putResp)
	}
	if len(putResp.Available) != 3 || putResp.Available[0] != "ask" {
		t.Errorf("PUT available = %v, want [ask auto full]", putResp.Available)
	}

	// PUT with an invalid mode → 400.
	body = bytes.NewBufferString(`{"mode":"nope"}`)
	req = httptest.NewRequest(http.MethodPut, "/v1/workspaces/permissions/foo", body)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid mode status = %d, want 400", rec.Code)
	}
}

// PermissionStore round-trips through the real settings writer: PUT persists to
// the workspace's settings.local.json and GET reads it back through the merge.
func TestPermissionStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	store := NewPermissionStore(home)

	if mode, err := store.EffectiveMode(root); err != nil || mode != "ask" {
		t.Fatalf("EffectiveMode on empty workspace = %q, %v; want ask, nil", mode, err)
	}
	if err := store.SetMode(root, "auto"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	// The file exists and carries the tier.
	path := settings.ProjectLocalPath(root)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("settings.local.json not written: %v", err)
	}
	if mode, err := store.EffectiveMode(root); err != nil || mode != "auto" {
		t.Fatalf("EffectiveMode after SetMode = %q, %v; want auto, nil", mode, err)
	}
	// A user-scope file is overridden by the workspace-local one.
	if err := settings.SetApprovalMode(settings.UserPath(home), "full"); err != nil {
		t.Fatalf("user SetApprovalMode: %v", err)
	}
	if mode, _ := store.EffectiveMode(root); mode != "auto" {
		t.Fatalf("EffectiveMode with user=full local=auto = %q, want auto (local wins)", mode)
	}
}

// The {path...} wildcard must carry an absolute workspace path with slashes.
func TestPermissionsAbsolutePath(t *testing.T) {
	svc := &fakePermissionService{mode: "ask"}
	h := NewMux(newFakeConversationRepo(), &fakeEventStore{}, nil, MuxOptions{Permissions: svc})
	srv := httptest.NewServer(h)
	defer srv.Close()

	abs := filepath.Join(t.TempDir(), "proj")
	resp, err := http.Get(srv.URL + "/v1/workspaces/permissions" + abs)
	if err != nil {
		t.Fatal(err)
	}
	var got permissionResponse
	decodeResponse(t, resp, &got)
	if got.Path != abs {
		t.Errorf("path = %q, want %q (multi-segment wildcard)", got.Path, abs)
	}
}

var _ conversation.ConversationRepository = (*fakeConversationRepo)(nil)