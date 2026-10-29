package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/collab"
	sessionsqlite "code-agent/internal/session/sqlite"
)

type managedFixtureService struct {
	ready bool
	calls int
}

func (s *managedFixtureService) Ready() bool { return s.ready }
func (s *managedFixtureService) StartRun(_ context.Context, a collab.Principal, _ string, _ collab.StartRunInput) (collab.Receipt, error) {
	s.calls++
	if a.ID != "runtime-owner" || !a.LocalSessions {
		return collab.Receipt{}, collab.ErrForbidden
	}
	return collab.Receipt{ID: "run", Status: "recorded"}, nil
}
func (*managedFixtureService) ReportReview(context.Context, collab.Principal, string, string, collab.ReviewInput) (collab.Receipt, error) {
	return collab.Receipt{}, collab.ErrForbidden
}
func (*managedFixtureService) Artifact(context.Context, collab.Principal, string, string) (collab.ReviewBundle, error) {
	return collab.ReviewBundle{}, collab.ErrForbidden
}

func TestManagedTaskHTTPRequiresReadyOwnerAndRejectsGenerationOverrides(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := &managedFixtureService{ready: true}
	auth := ServerAuth{Enabled: true, Token: "local-test-owner-token-0123456789abcdef"}
	handler := WithManagedTasks(http.NotFoundHandler(), store.TaskRepository("server"), svc, auth)
	call := func(method, path, token, body string, shared bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if shared {
			req = withSharedDeviceAuthContext(req)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if got := call("GET", "/v1/tasks/capabilities", "", "", false); got.Code != 401 {
		t.Fatal(got.Code)
	}
	if got := call("GET", "/v1/tasks/capabilities", "", "", true); got.Code != 403 {
		t.Fatal("shared device gained owner access", got.Code)
	}
	if got := call("GET", "/v1/tasks/capabilities", auth.Token, "", false); got.Code != 200 || !strings.Contains(got.Body.String(), "task_review_gate_v1") {
		t.Fatal(got.Code, got.Body.String())
	}
	for _, field := range []string{"model", "max_tokens", "thinking", "workflow", "protected_paths"} {
		if got := call("POST", "/v1/tasks/task/runs", auth.Token, `{"request_id":"run","`+field+`":"override"}`, false); got.Code != 400 || svc.calls != 0 {
			t.Fatal("internal host control exposed", field, got.Code)
		}
	}
	if got := call("POST", "/v1/tasks/task/runs", auth.Token, `{"request_id":"run"}`, false); got.Code != 200 || svc.calls != 1 {
		t.Fatal(got.Code, svc.calls)
	}
	svc.ready = false
	if got := call("GET", "/v1/tasks/capabilities", auth.Token, "", false); got.Code != 503 {
		t.Fatal("lost readiness still advertised", got.Code)
	}
	base := WithManagedTasks(http.NotFoundHandler(), store.TaskRepository("server"), nil, auth)
	w := httptest.NewRecorder()
	base.ServeHTTP(w, httptest.NewRequest("POST", "/v1/tasks/task/runs", nil))
	if w.Code != 404 {
		t.Fatal("unassembled managed service exposed", w.Code)
	}
}
