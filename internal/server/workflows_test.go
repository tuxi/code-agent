package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	runtime "code-agent/internal/runtime"
)

// fakeWorkflowFuncs returns simple stubs that echo their inputs so the routes
// can assert path/param extraction and JSON shape without a real DB.
func fakeWorkflowFuncs(t *testing.T) *MuxOptions {
	t.Helper()
	opts := &MuxOptions{
		WorkflowList: func(ctx context.Context, root string) ([]runtime.WorkflowSummary, error) {
			return []runtime.WorkflowSummary{{ID: 1, Name: "wf-a", LatestStatus: "success"}}, nil
		},
		WorkflowDetail: func(ctx context.Context, root, name string) (*runtime.WorkflowDetail, error) {
			return &runtime.WorkflowDetail{ID: 1, Name: name, Description: "root=" + root}, nil
		},
		WorkflowSnapshotByTask: func(ctx context.Context, root string, taskID int64) (*runtime.WorkflowSnapshot, error) {
			return &runtime.WorkflowSnapshot{
				WorkflowID: "wf-a",
				Task:       &runtime.WorkflowTaskState{ID: taskID, Status: "pending"},
			}, nil
		},
		WorkflowRun: func(ctx context.Context, root, name string, input map[string]any) (int64, error) {
			return 42, nil
		},
	}
	return opts
}

func newWorkflowMux(t *testing.T, opts *MuxOptions) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	registerWorkflowRoutes(mux, *opts)
	return mux
}

func TestWorkflowRoutes(t *testing.T) {
	mux := newWorkflowMux(t, fakeWorkflowFuncs(t))
	type envelope struct {
		Data json.RawMessage `json:"data"`
	}
	ws := "?workspace=" + url.QueryEscape("/Users/xiaoyuan/work")
	get := func(t *testing.T, path string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		var env envelope
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: unmarshal envelope: %v body=%s", path, err, rr.Body.String())
		}
		return env.Data
	}

	t.Run("list", func(t *testing.T) {
		raw := get(t, "/v1/workflows"+ws)
		var items []runtime.WorkflowSummary
		if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 {
			t.Fatalf("body=%s", string(raw))
		}
	})

	t.Run("detail", func(t *testing.T) {
		raw := get(t, "/v1/workflows/wf-a"+ws)
		var d runtime.WorkflowDetail
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		// The query-param workspace path must decode back to the absolute path.
		if d.Description != "root=/Users/xiaoyuan/work" || d.Name != "wf-a" {
			t.Fatalf("detail=%+v", d)
		}
	})

	t.Run("headless snapshot by task", func(t *testing.T) {
		raw := get(t, "/v1/workflows/wf-a/runs/42/snapshot"+ws)
		var s runtime.WorkflowSnapshot
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		if s.Task == nil || s.Task.ID != 42 {
			t.Fatalf("snapshot=%+v", s)
		}
	})

	t.Run("missing workspace param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/workflows", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", rr.Code)
		}
	})

	t.Run("headless trigger run", func(t *testing.T) {
		body := bytes.NewReader([]byte(`{"goal":"g","agents":[],"parallelism":1}`))
		req := httptest.NewRequest(http.MethodPost, "/v1/workflows/wf-a/runs"+ws, body)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Data map[string]int64 `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Data["task_id"] != 42 {
			t.Fatalf("body=%s", rr.Body.String())
		}
	})

	t.Run("headless trigger bad body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/workflows/wf-a/runs"+ws, bytes.NewReader([]byte(`{`)))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", rr.Code)
		}
	})

	t.Run("bad task id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/workflows/wf-a/runs/abc/snapshot"+ws, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status=%d", rr.Code)
		}
	})
}

func TestWorkflowRoutesDisabledWhenNil(t *testing.T) {
	mux := http.NewServeMux()
	registerWorkflowRoutes(mux, MuxOptions{}) // WorkflowList nil
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows?workspace=/foo", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rr.Code)
	}
}
