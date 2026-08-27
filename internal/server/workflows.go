package server

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// registerWorkflowRoutes serves the workspace-scoped workflow catalog and
// observation endpoints (/v1/workflows...). These routes use a query parameter
// for the workspace path instead of a URL wildcard, because Go's ServeMux
// conflicts a single-segment {path} wildcard with the existing
// /v1/workspaces/permissions/{path...} route (both match
// /v1/workspaces/permissions/workflows and neither is more specific).
//
// A nil WorkflowList leaves the endpoints absent (404), matching the
// AutomationStore/Providers pattern. {name} is the workflow's registered
// definition name; for plan_workflow runs that is the hash-derived identity,
// for future tool_sequence templates it is the user-chosen name.
func registerWorkflowRoutes(mux *http.ServeMux, opts MuxOptions) {
	if opts.WorkflowList == nil {
		return
	}

	workspaceRoot := func(r *http.Request) (string, bool) {
		v := r.URL.Query().Get("workspace")
		if v == "" {
			return "", false
		}
		return normalizeWorkspacePath(v), true
	}

	mux.HandleFunc("GET /v1/workflows", func(w http.ResponseWriter, r *http.Request) {
		root, ok := workspaceRoot(r)
		if !ok {
			http.Error(w, "missing workspace query parameter", http.StatusBadRequest)
			return
		}
		items, err := opts.WorkflowList(r.Context(), root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, r, http.StatusOK, items)
	})

	mux.HandleFunc("GET /v1/workflows/{name}", func(w http.ResponseWriter, r *http.Request) {
		root, ok := workspaceRoot(r)
		if !ok {
			http.Error(w, "missing workspace query parameter", http.StatusBadRequest)
			return
		}
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "missing workflow name", http.StatusBadRequest)
			return
		}
		d, err := opts.WorkflowDetail(r.Context(), root, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, r, http.StatusOK, d)
	})

	// Headless observation surface (R2): a run is identified by its task id
	// (the value POST /runs returns), not a conversation id. The workspace
	// path is a query parameter so the URL stays free of ServeMux wildcard
	// conflicts with the existing /v1/workspaces/ routes.
	mux.HandleFunc("GET /v1/workflows/{name}/runs/{task_id}/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if opts.WorkflowSnapshotByTask == nil {
			http.Error(w, "workflow snapshot not available", http.StatusNotFound)
			return
		}
		root, ok := workspaceRoot(r)
		if !ok {
			http.Error(w, "missing workspace query parameter", http.StatusBadRequest)
			return
		}
		taskID, err := strconv.ParseInt(r.PathValue("task_id"), 10, 64)
		if err != nil || taskID <= 0 {
			http.Error(w, "invalid task_id", http.StatusBadRequest)
			return
		}
		snapshot, err := opts.WorkflowSnapshotByTask(r.Context(), root, taskID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, r, http.StatusOK, snapshot)
	})

	// Headless trigger (R5): submit a saved template by name and return the
	// task id. The run executes asynchronously in the workspace runtime's
	// workers; the client observes progress via the snapshot endpoint and the
	// workflow event stream.
	mux.HandleFunc("POST /v1/workflows/{name}/runs", func(w http.ResponseWriter, r *http.Request) {
		if opts.WorkflowRun == nil {
			http.Error(w, "workflow run not available", http.StatusNotFound)
			return
		}
		root, ok := workspaceRoot(r)
		if !ok {
			http.Error(w, "missing workspace query parameter", http.StatusBadRequest)
			return
		}
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "missing workflow name", http.StatusBadRequest)
			return
		}
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		taskID, err := opts.WorkflowRun(r.Context(), root, name, input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, r, http.StatusAccepted, map[string]int64{"task_id": taskID})
	})
}
