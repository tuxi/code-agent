package server

import (
	"net/http"
	"strings"

	"code-agent/internal/collab"
)

// WithManagedTasks adds only explicitly assembled, reconciled capabilities.
// The existing handler and manual Session API retain their own auth/policy.
func WithManagedTasks(base http.Handler, repo collab.Repository, svc collab.ManagedService, auth ServerAuth) http.Handler {
	if repo == nil || svc == nil || !svc.Ready() {
		return base
	}
	mux := http.NewServeMux()
	type operation func(http.ResponseWriter, *http.Request, collab.Principal) (any, error)
	handle := func(pattern string, op operation) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			owner, _ := r.Context().Value(runtimeOwnerAuthContextKey{}).(bool)
			if !owner {
				writeTaskError(w, r, collab.ErrForbidden)
				return
			}
			if !svc.Ready() {
				writeTaskError(w, r, collab.ErrUnavailable)
				return
			}
			a := collab.Principal{ID: "runtime-owner", LocalSessions: true}
			value, err := op(w, r, a)
			if err != nil {
				writeTaskError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, value)
		})
	}
	handle("GET /v1/tasks/capabilities", func(_ http.ResponseWriter, r *http.Request, _ collab.Principal) (any, error) {
		id, err := repo.Identity(r.Context())
		if err != nil {
			return nil, err
		}
		return struct {
			collab.Identity
			Capabilities        []string `json:"capabilities"`
			Projection          string   `json:"projection"`
			ExecutionProfile    string   `json:"execution_profile"`
			UnknownSideEffects  string   `json:"unknown_side_effects"`
			AutomaticTurnReplay bool     `json:"automatic_turn_replay"`
		}{id, []string{"task_links_v1", "task_timeline_v1", "task_managed_workflow_v1", "task_review_gate_v1"}, "read_through", "local_foreground_v1", "recovery_required", false}, nil
	})
	handle("POST /v1/tasks/{id}/runs", func(w http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		var in collab.StartRunInput
		if err := decodeTaskInput(w, r, &in); err != nil {
			return nil, err
		}
		return svc.StartRun(r.Context(), a, r.PathValue("id"), in)
	})
	handle("POST /v1/tasks/{id}/runs/{run}/reviews", func(w http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		var in collab.ReviewInput
		if err := decodeTaskInput(w, r, &in); err != nil {
			return nil, err
		}
		return svc.ReportReview(r.Context(), a, r.PathValue("id"), r.PathValue("run"), in)
	})
	handle("GET /v1/tasks/{id}/immutable-artifacts/{artifact}", func(_ http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		return svc.Artifact(r.Context(), a, r.PathValue("id"), r.PathValue("artifact"))
	})
	managed := withServerAuth(mux, auth)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/v1/tasks/capabilities" || managedTaskPath(p) {
			managed.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

func managedTaskPath(path string) bool {
	// Match only the added shapes. Other Task and all Session routes continue
	// through the original mux; unknown actions do not gain a recovery API.
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "v1" && parts[1] == "tasks" && parts[3] == "runs" || len(parts) == 6 && parts[0] == "v1" && parts[1] == "tasks" && parts[3] == "runs" && parts[5] == "reviews" || len(parts) == 5 && parts[0] == "v1" && parts[1] == "tasks" && parts[3] == "immutable-artifacts"
}
