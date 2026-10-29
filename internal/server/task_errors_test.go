package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"code-agent/internal/collab"
)

func TestTaskBoundaryErrorsPreserveCodesWithoutPrivateDetails(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   string
		status int
	}{
		{collab.ErrRecoveryRequired, "recovery_required", 409},
		{collab.ErrManagedWorkspace, "managed_workspace", 409},
		{collab.ErrConflict, "task_conflict", 409},
		{collab.ErrExecutorModelUnavailable, "executor_model_unavailable", 409},
	} {
		w := httptest.NewRecorder()
		writeTaskError(w, httptest.NewRequest("POST", "/v1/tasks/test/runs", nil), fmt.Errorf("private storage details: %w", tc.err))
		var out struct {
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if w.Code != tc.status || out.Data.Code != tc.code || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
