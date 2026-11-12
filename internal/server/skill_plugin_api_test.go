package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	runtimepkg "code-agent/internal/runtime"
)

const managementTestToken = "0123456789abcdef0123456789abcdef"

type fakeSkillPluginService struct {
	skillInventory  runtimepkg.SkillInventory
	pluginInventory runtimepkg.PluginInventory
	mutation        runtimepkg.PluginMutation
	uninstall       runtimepkg.UninstallPluginMutation
	pluginErr       error
	gotSessionID    string
	gotWorkspace    string
	gotAction       string
	gotInstall      runtimepkg.InstallLocalPluginRequest
}

func (f *fakeSkillPluginService) SkillInventory(_ context.Context, sessionID, workspacePath string) (runtimepkg.SkillInventory, error) {
	f.gotSessionID, f.gotWorkspace = sessionID, workspacePath
	inventory := f.skillInventory
	inventory.Scope.SessionID = sessionID
	return inventory, nil
}

func (f *fakeSkillPluginService) PluginInventory(context.Context) (runtimepkg.PluginInventory, error) {
	return f.pluginInventory, f.pluginErr
}

func (f *fakeSkillPluginService) SetPluginEnabled(_ context.Context, _ string, enabled bool) (runtimepkg.PluginMutation, error) {
	f.gotAction = "disable"
	if enabled {
		f.gotAction = "enable"
	}
	result := f.mutation
	result.Plugin.Enabled = enabled
	result.Action = f.gotAction + "d"
	return result, f.pluginErr
}

func (f *fakeSkillPluginService) InstallLocalPlugin(_ context.Context, req runtimepkg.InstallLocalPluginRequest) (runtimepkg.PluginMutation, error) {
	f.gotInstall = req
	if !req.Confirmed {
		return runtimepkg.PluginMutation{}, &runtimepkg.ManagementError{
			Code: runtimepkg.CodePluginConfirmationRequired, Message: "explicit confirmation is required to install a local plugin",
		}
	}
	result := f.mutation
	result.Action = "installed"
	return result, f.pluginErr
}

func (f *fakeSkillPluginService) UninstallPlugin(_ context.Context, name string) (runtimepkg.UninstallPluginMutation, error) {
	f.gotAction = "uninstall:" + name
	return f.uninstall, f.pluginErr
}

func TestSkillPluginManagementAuthCapabilityAndWorkspaceResolution(t *testing.T) {
	repo := newFakeConversationRepo()
	session, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeSkillPluginService{}
	service.skillInventory = readSkillInventoryFixture(t)

	mux := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{
		ServerAuth:   ServerAuth{Enabled: true, Token: managementTestToken, PublicHealthz: true},
		SkillPlugins: service,
	})

	for _, tc := range []struct{ name, header string }{
		{name: "missing"},
		{name: "invalid", header: "Bearer wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveManagementRequest(mux, http.MethodGet, "/v1/runtime/skills?session_id="+session.ID, nil, tc.header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	rec := serveManagementRequest(mux, http.MethodGet, "/v1/runtime/skills?session_id="+session.ID, nil, "Bearer "+managementTestToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if service.gotSessionID != session.ID || service.gotWorkspace != session.WorkspacePath {
		t.Fatalf("workspace resolution = session %q workspace %q", service.gotSessionID, service.gotWorkspace)
	}
	data := envelopeData(t, rec)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	wantJSON, err := os.ReadFile("testdata/skill_inventory_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(wantJSON, &want); err != nil {
		t.Fatal(err)
	}
	want["scope"].(map[string]any)["session_id"] = session.ID
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skill inventory shape mismatch\ngot  %#v\nwant %#v", got, want)
	}

	rec = serveManagementRequest(mux, http.MethodGet, "/v1/runtime/skills?session_id="+session.ID+"&workspace_path=/etc", nil, "Bearer "+managementTestToken)
	assertManagementError(t, rec, http.StatusBadRequest, "workspace_scope_unsupported")

	rec = serveManagementRequest(mux, http.MethodGet, "/v1/runtime/skills?session_id=missing", nil, "Bearer "+managementTestToken)
	assertManagementError(t, rec, http.StatusNotFound, "conversation_not_found")

	rec = serveManagementRequest(mux, http.MethodGet, "/v1/runtime/capabilities", nil, "Bearer "+managementTestToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d", rec.Code)
	}
	var caps struct {
		Capabilities RuntimeCapabilities `json:"capabilities"`
	}
	if err := json.Unmarshal(envelopeData(t, rec), &caps); err != nil {
		t.Fatal(err)
	}
	if !caps.Capabilities.SkillPluginManagement {
		t.Fatal("skill_plugin_management_v1 capability not advertised")
	}

	rec = serveManagementRequest(mux, http.MethodGet, "/v1/runtime/plugins", nil, "")
	assertManagementError(t, rec, http.StatusUnauthorized, "runtime_auth_required")

	unauthenticated := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{SkillPlugins: service})
	rec = serveManagementRequest(unauthenticated, http.MethodGet, "/v1/runtime/plugins", nil, "")
	assertManagementError(t, rec, http.StatusUnauthorized, "runtime_auth_required")
}

func TestSkillPluginManagementPluginRoutesAndErrors(t *testing.T) {
	repo := newFakeConversationRepo()
	service := &fakeSkillPluginService{}
	service.pluginInventory = readPluginInventoryFixture(t)
	service.mutation = readPluginMutationFixture(t)
	service.uninstall = runtimepkg.UninstallPluginMutation{
		Schema: runtimepkg.PluginMutationSchemaV1, Action: "uninstalled", Name: "docs", Version: "1.0.0",
		ReloadRequired: true, ReloadHint: runtimepkg.PluginReloadHint,
	}
	mux := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{
		ServerAuth: ServerAuth{Enabled: true, Token: managementTestToken}, SkillPlugins: service,
	})
	auth := "Bearer " + managementTestToken

	rec := serveManagementRequest(mux, http.MethodGet, "/v1/runtime/plugins", nil, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(jsonValue(t, envelopeData(t, rec)), jsonValue(t, mustReadFile(t, "testdata/plugin_inventory_response.json"))) {
		t.Fatal("plugin inventory response did not match fixture")
	}

	rec = serveManagementRequest(mux, http.MethodPost, "/v1/runtime/plugins/install-local", strings.NewReader(`{"source":"/tmp/plugin"}`), auth)
	assertManagementError(t, rec, http.StatusPreconditionRequired, runtimepkg.CodePluginConfirmationRequired)

	rec = serveManagementRequest(mux, http.MethodPost, "/v1/runtime/plugins/install-local", strings.NewReader(`{"source":"/tmp/plugin","confirmed":true}`), auth)
	if rec.Code != http.StatusCreated {
		t.Fatalf("install status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !service.gotInstall.Confirmed || service.gotInstall.Source != "/tmp/plugin" {
		t.Fatalf("install request = %#v", service.gotInstall)
	}

	rec = serveManagementRequest(mux, http.MethodPost, "/v1/runtime/plugins/docs/disable", nil, auth)
	if rec.Code != http.StatusOK || service.gotAction != "disable" {
		t.Fatalf("disable status=%d action=%q body=%s", rec.Code, service.gotAction, rec.Body.String())
	}
	if !reflect.DeepEqual(jsonValue(t, envelopeData(t, rec)), jsonValue(t, mustReadFile(t, "testdata/plugin_mutation_response.json"))) {
		t.Fatalf("disable response did not match fixture: %s", rec.Body.String())
	}
	rec = serveManagementRequest(mux, http.MethodPost, "/v1/runtime/plugins/docs/enable", nil, auth)
	if rec.Code != http.StatusOK || service.gotAction != "enable" {
		t.Fatalf("enable status=%d action=%q body=%s", rec.Code, service.gotAction, rec.Body.String())
	}
	rec = serveManagementRequest(mux, http.MethodDelete, "/v1/runtime/plugins/docs", nil, auth)
	if rec.Code != http.StatusOK || service.gotAction != "uninstall:docs" {
		t.Fatalf("uninstall status=%d action=%q body=%s", rec.Code, service.gotAction, rec.Body.String())
	}

	rec = serveManagementRequest(mux, http.MethodPost, "/v1/runtime/plugins/install-local", strings.NewReader(`{"source":`), auth)
	assertManagementError(t, rec, http.StatusBadRequest, "invalid_request")

	service.pluginErr = &runtimepkg.ManagementError{
		Code: runtimepkg.CodePluginIntegrityError, Message: "installed plugin failed integrity verification",
		Cause: errors.New("/Users/private/.codeagent/plugins/cache/docs: digest mismatch"),
	}
	rec = serveManagementRequest(mux, http.MethodGet, "/v1/runtime/plugins", nil, auth)
	assertManagementError(t, rec, http.StatusConflict, runtimepkg.CodePluginIntegrityError)
	if strings.Contains(rec.Body.String(), "/Users/private") || strings.Contains(rec.Body.String(), "digest mismatch") {
		t.Fatalf("management error leaked internal cause: %s", rec.Body.String())
	}
	wantError := jsonValue(t, mustReadFile(t, "testdata/management_error_response.json"))
	if !reflect.DeepEqual(jsonValue(t, envelopeData(t, rec)), wantError) {
		t.Fatalf("management error fixture mismatch: %s", rec.Body.String())
	}
}

func readSkillInventoryFixture(t *testing.T) runtimepkg.SkillInventory {
	t.Helper()
	var inventory runtimepkg.SkillInventory
	if err := json.Unmarshal(mustReadFile(t, "testdata/skill_inventory_response.json"), &inventory); err != nil {
		t.Fatal(err)
	}
	return inventory
}

func readPluginInventoryFixture(t *testing.T) runtimepkg.PluginInventory {
	t.Helper()
	var inventory runtimepkg.PluginInventory
	if err := json.Unmarshal(mustReadFile(t, "testdata/plugin_inventory_response.json"), &inventory); err != nil {
		t.Fatal(err)
	}
	return inventory
}

func readPluginMutationFixture(t *testing.T) runtimepkg.PluginMutation {
	t.Helper()
	var mutation runtimepkg.PluginMutation
	if err := json.Unmarshal(mustReadFile(t, "testdata/plugin_mutation_response.json"), &mutation); err != nil {
		t.Fatal(err)
	}
	return mutation
}

func serveManagementRequest(handler http.Handler, method, path string, body *strings.Reader, auth string) *httptest.ResponseRecorder {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, body)
	}
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	return rec
}

func envelopeData(t *testing.T, rec *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func assertManagementError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, status, rec.Body.String())
	}
	var envelope struct {
		Code int                     `json:"code"`
		Data managementErrorResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != status*100 || envelope.Data.Code != code {
		t.Fatalf("error envelope = %+v", envelope)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func jsonValue(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
