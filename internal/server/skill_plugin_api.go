package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"code-agent/internal/conversation"
	runtimepkg "code-agent/internal/runtime"
)

// SkillPluginService is the runtime-owned management boundary. Implementations
// receive workspace paths only from a loaded conversation, never directly from
// request parameters.
type SkillPluginService interface {
	SkillInventory(ctx context.Context, sessionID, workspacePath string) (runtimepkg.SkillInventory, error)
	PluginInventory(ctx context.Context) (runtimepkg.PluginInventory, error)
	SetPluginEnabled(ctx context.Context, name string, enabled bool) (runtimepkg.PluginMutation, error)
	InstallLocalPlugin(ctx context.Context, req runtimepkg.InstallLocalPluginRequest) (runtimepkg.PluginMutation, error)
	UninstallPlugin(ctx context.Context, name string) (runtimepkg.UninstallPluginMutation, error)
}

type managementErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func registerSkillPluginRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, opts MuxOptions) {
	if opts.SkillPlugins == nil {
		return
	}
	service := opts.SkillPlugins

	mux.HandleFunc("GET /v1/runtime/skills", func(w http.ResponseWriter, r *http.Request) {
		if !managementAuthReady(w, r, opts.ServerAuth) {
			return
		}
		if r.URL.Query().Has("workspace_path") {
			writeManagementError(w, r, &runtimepkg.ManagementError{
				Code:    "workspace_scope_unsupported",
				Message: "workspace_path is not accepted; use session_id so project scope is resolved by the runtime",
			})
			return
		}
		sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
		workspacePath := ""
		if sessionID != "" {
			if repo == nil {
				writeManagementError(w, r, &runtimepkg.ManagementError{Code: "conversation_not_found", Message: "conversation was not found"})
				return
			}
			session, err := loadConversationHeader(r.Context(), repo, sessionID)
			if err != nil {
				writeManagementError(w, r, &runtimepkg.ManagementError{Code: "conversation_not_found", Message: "conversation was not found"})
				return
			}
			if needsRebind, err := repo.NeedsRebind(r.Context(), sessionID); err == nil && needsRebind {
				writeManagementError(w, r, &runtimepkg.ManagementError{Code: "workspace_scope_unresolved", Message: "conversation workspace must be rebound before skills can be inspected"})
				return
			}
			if strings.TrimSpace(session.WorkspacePath) == "" {
				writeManagementError(w, r, &runtimepkg.ManagementError{Code: "workspace_scope_unresolved", Message: "conversation has no resolved workspace"})
				return
			}
			workspacePath = session.WorkspacePath
		}
		inventory, err := service.SkillInventory(r.Context(), sessionID, workspacePath)
		if err != nil {
			writeManagementError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, inventory)
	})

	mux.HandleFunc("GET /v1/runtime/plugins", func(w http.ResponseWriter, r *http.Request) {
		if !managementAuthReady(w, r, opts.ServerAuth) {
			return
		}
		inventory, err := service.PluginInventory(r.Context())
		if err != nil {
			writeManagementError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, inventory)
	})

	mux.HandleFunc("POST /v1/runtime/plugins/install-local", func(w http.ResponseWriter, r *http.Request) {
		if !managementAuthReady(w, r, opts.ServerAuth) {
			return
		}
		var req runtimepkg.InstallLocalPluginRequest
		if err := decodeBoundedJSON(w, r, &req, 64<<10); err != nil {
			writeManagementError(w, r, &runtimepkg.ManagementError{Code: "invalid_request", Message: "invalid JSON body"})
			return
		}
		result, err := service.InstallLocalPlugin(r.Context(), req)
		if err != nil {
			writeManagementError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, result)
	})

	mux.HandleFunc("POST /v1/runtime/plugins/{name}/enable", func(w http.ResponseWriter, r *http.Request) {
		if !managementAuthReady(w, r, opts.ServerAuth) {
			return
		}
		result, err := service.SetPluginEnabled(r.Context(), r.PathValue("name"), true)
		if err != nil {
			writeManagementError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, result)
	})

	mux.HandleFunc("POST /v1/runtime/plugins/{name}/disable", func(w http.ResponseWriter, r *http.Request) {
		if !managementAuthReady(w, r, opts.ServerAuth) {
			return
		}
		result, err := service.SetPluginEnabled(r.Context(), r.PathValue("name"), false)
		if err != nil {
			writeManagementError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, result)
	})

	mux.HandleFunc("DELETE /v1/runtime/plugins/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !managementAuthReady(w, r, opts.ServerAuth) {
			return
		}
		result, err := service.UninstallPlugin(r.Context(), r.PathValue("name"))
		if err != nil {
			writeManagementError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, result)
	})
}

func managementAuthReady(w http.ResponseWriter, r *http.Request, auth ServerAuth) bool {
	if auth.Enabled {
		return true
	}
	Result(w, r, http.StatusUnauthorized, CodeUnauthorized, "unauthorized", runtimeAuthError{
		Code: "runtime_auth_required", Message: "Management APIs require bearer authentication",
	})
	return false
}

func decodeBoundedJSON(w http.ResponseWriter, r *http.Request, target any, limit int64) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeManagementError(w http.ResponseWriter, r *http.Request, err error) {
	var management *runtimepkg.ManagementError
	if !errors.As(err, &management) {
		Result(w, r, http.StatusInternalServerError, CodeInternal, "skill/plugin management failed", managementErrorResponse{
			Code: "management_failed", Message: "skill/plugin management failed",
		})
		return
	}
	status := http.StatusInternalServerError
	switch management.Code {
	case "invalid_request", runtimepkg.CodePluginSourceInvalid, runtimepkg.CodePluginSourceUnsupported:
		status = http.StatusBadRequest
	case "workspace_scope_unsupported":
		status = http.StatusBadRequest
	case "conversation_not_found", runtimepkg.CodePluginNotFound:
		status = http.StatusNotFound
	case "workspace_scope_unresolved", runtimepkg.CodePluginIntegrityError, runtimepkg.CodePluginCachePathUnsafe:
		status = http.StatusConflict
	case runtimepkg.CodePluginConfirmationRequired:
		status = http.StatusPreconditionRequired
	case runtimepkg.CodePluginManifestInvalid:
		status = http.StatusUnprocessableEntity
	case runtimepkg.CodePluginRegistryInvalid, runtimepkg.CodePluginOperationFailed, runtimepkg.CodeSkillInventoryFailed:
		status = http.StatusInternalServerError
	}
	Result(w, r, status, status*100, management.Message, managementErrorResponse{
		Code: management.Code, Message: management.Message,
	})
}
