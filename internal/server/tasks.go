package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"code-agent/internal/collab"
	"code-agent/internal/conversation"
)

// Task routes are optional and owner-only in B1. Shared-device and anonymous
// traffic has no implicit Task/reporting authority. The auth middleware sets
// identity after credential verification; body fields never select an actor.
func registerTaskRoutes(mux *http.ServeMux, conversations conversation.ConversationRepository, serverID string) bool {
	port, ok := conversations.(collab.RepositoryProvider)
	if !ok {
		return false
	}
	repo := port.TaskRepository(serverID)
	if repo == nil {
		return false
	}
	actor := func(r *http.Request) (collab.Principal, error) {
		if owner, _ := r.Context().Value(runtimeOwnerAuthContextKey{}).(bool); !owner {
			return collab.Principal{}, collab.ErrForbidden
		}
		return collab.Principal{ID: "runtime-owner", LocalSessions: true}, nil
	}
	type operation func(http.ResponseWriter, *http.Request, collab.Principal) (any, error)
	handle := func(pattern string, op operation) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			a, err := actor(r)
			if err != nil {
				writeTaskError(w, r, err)
				return
			}
			value, err := op(w, r, a)
			if err != nil {
				writeTaskError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, value)
		})
	}
	handle("GET /v1/tasks/capabilities", func(_ http.ResponseWriter, r *http.Request, _ collab.Principal) (any, error) {
		i, err := repo.Identity(r.Context())
		if err != nil {
			return nil, err
		}
		return struct {
			collab.Identity
			Capabilities []string `json:"capabilities"`
			Projection   string   `json:"projection"`
		}{i, []string{"task_links_v1", "task_timeline_v1"}, "read_through"}, nil
	})
	handle("POST /v1/task-agent-sessions", func(w http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		var in collab.ExternalInput
		if err := decodeTaskInput(w, r, &in); err != nil {
			return nil, err
		}
		return repo.RegisterExternal(r.Context(), a, in)
	})
	handle("POST /v1/tasks", func(w http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		var in collab.CreateInput
		if err := decodeTaskInput(w, r, &in); err != nil {
			return nil, err
		}
		return repo.Create(r.Context(), a, in)
	})
	handle("GET /v1/tasks", func(_ http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		limit, err := taskPageLimit(r)
		if err != nil {
			return nil, err
		}
		return repo.List(r.Context(), a, r.URL.Query().Get("workspace_ref"), limit)
	})
	handle("GET /v1/tasks/{id}", func(_ http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		snapshot, err := repo.Snapshot(r.Context(), a, r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		// Optional Bridge restrictions only narrow owner access. Check before
		// serializing any plan/artifact content into the response.
		if expected := r.URL.Query().Get("scope_workspace_ref"); expected != "" && snapshot.WorkspaceRef != expected {
			return nil, collab.ErrForbidden
		}
		if expected := r.URL.Query().Get("scope_session_id"); expected != "" {
			for _, p := range snapshot.Participations {
				if p.SessionRef.Kind == "local" && p.SessionRef.SessionID != expected {
					return nil, collab.ErrForbidden
				}
			}
		}
		return snapshot, nil
	})
	handle("GET /v1/tasks/{id}/events", func(_ http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		limit, err := taskPageLimit(r)
		if err != nil {
			return nil, err
		}
		after := int64(0)
		if raw, ok := r.URL.Query()["after"]; ok {
			if len(raw) != 1 || raw[0] == "" {
				return nil, collab.ErrInvalid
			}
			after, err = strconv.ParseInt(raw[0], 10, 64)
			if err != nil || after < 0 {
				return nil, collab.ErrInvalid
			}
		}
		return repo.Events(r.Context(), a, r.PathValue("id"), after, limit)
	})
	handle("POST /v1/tasks/{id}/participations", func(w http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		var in collab.ParticipationInput
		if err := decodeTaskInput(w, r, &in); err != nil {
			return nil, err
		}
		return repo.Participate(r.Context(), a, r.PathValue("id"), in)
	})
	handle("POST /v1/tasks/{id}/attempts", func(w http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		var in collab.AttemptInput
		if err := decodeTaskInput(w, r, &in); err != nil {
			return nil, err
		}
		return repo.LinkAttempt(r.Context(), a, r.PathValue("id"), in)
	})
	handle("POST /v1/tasks/{id}/artifacts", func(w http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
		var in collab.ArtifactInput
		if err := decodeTaskInput(w, r, &in); err != nil {
			return nil, err
		}
		return repo.LinkArtifact(r.Context(), a, r.PathValue("id"), in)
	})
	for _, kind := range []string{"plan", "review"} {
		handle("POST /v1/tasks/{id}/"+kind+"s", func(w http.ResponseWriter, r *http.Request, a collab.Principal) (any, error) {
			var in collab.ReportInput
			if err := decodeTaskInput(w, r, &in); err != nil {
				return nil, err
			}
			return repo.Report(r.Context(), a, r.PathValue("id"), kind, in)
		})
	}
	return true
}
func decodeTaskInput(w http.ResponseWriter, r *http.Request, value any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(value) != nil {
		return collab.ErrInvalid
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return collab.ErrInvalid
	}
	return nil
}
func taskPageLimit(r *http.Request) (int, error) {
	if raw, ok := r.URL.Query()["limit"]; ok {
		if len(raw) != 1 || raw[0] == "" {
			return 0, collab.ErrInvalid
		}
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > 200 {
			return 0, collab.ErrInvalid
		}
		return n, nil
	}
	return 100, nil
}
func writeTaskError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, "task_storage_error"
	switch {
	case errors.Is(err, collab.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid_task_request"
	case errors.Is(err, collab.ErrNotFound):
		status, code = http.StatusNotFound, "task_resource_not_found"
	case errors.Is(err, collab.ErrUnavailable):
		status, code = http.StatusServiceUnavailable, "managed_unavailable"
	case errors.Is(err, collab.ErrConflict):
		status, code = http.StatusConflict, "task_conflict"
	case errors.Is(err, collab.ErrForbidden):
		status, code = http.StatusForbidden, "task_forbidden"
	}
	writeJSON(w, r, status, map[string]string{"code": code})
}
