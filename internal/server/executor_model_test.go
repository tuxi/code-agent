package server

import (
	"code-agent/internal/collab"
	sessionsqlite "code-agent/internal/session/sqlite"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type modelFixtureService struct {
	managedFixtureService
	selected    string
	unavailable bool
}

func (*modelFixtureService) SupportsExecutorModel() bool { return true }
func (s *modelFixtureService) StartRun(_ context.Context, _ collab.Principal, _ string, in collab.StartRunInput) (collab.Receipt, error) {
	s.selected = in.ExecutorModel
	if s.unavailable {
		return collab.Receipt{}, collab.ErrExecutorModelUnavailable
	}
	return collab.Receipt{ID: "run"}, nil
}

func TestManagedHTTPExecutorModelCapabilityAndError(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	auth := ServerAuth{Enabled: true, Token: "offline-owner-token-0123456789abcdef"}
	modern := &modelFixtureService{managedFixtureService: managedFixtureService{ready: true}}
	for _, svc := range []collab.ManagedService{&managedFixtureService{ready: true}, modern} {
		h := WithManagedTasks(http.NotFoundHandler(), store.TaskRepository("server"), svc, auth)
		call := func(method, path, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+auth.Token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w
		}
		caps := call("GET", "/v1/tasks/capabilities", "")
		supports := svc == modern
		if strings.Contains(caps.Body.String(), "task_executor_model_v1") != supports {
			t.Fatal(caps.Body.String())
		}
		w := call("POST", "/v1/tasks/task/runs", `{"request_id":"run","executor_model":"offline/selected"}`)
		if !supports {
			if w.Code != 503 {
				t.Fatal(w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != 200 || modern.selected != "offline/selected" {
			t.Fatal(w.Code, modern.selected)
		}
		modern.unavailable = true
		w = call("POST", "/v1/tasks/task/runs", `{"executor_model":"offline/missing"}`)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "executor_model_unavailable") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
