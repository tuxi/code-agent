package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"code-agent/internal/gitworkspace"
)

type gitBranchListRequest struct {
	WorkspacePath string `json:"workspace_path"`
}
type gitBranchCreateRequest struct {
	WorkspacePath   string  `json:"workspace_path"`
	Name            string  `json:"name"`
	StartPoint      *string `json:"start_point"`
	Checkout        bool    `json:"checkout"`
	ClientRequestID string  `json:"client_request_id,omitempty"`
}
type gitBranchCheckoutRequest struct {
	WorkspacePath string `json:"workspace_path"`
	Name          string `json:"name"`
	AllowDirty    bool   `json:"allow_dirty"`
}

func registerGitBranchRoutes(mux *http.ServeMux, opts MuxOptions) {
	if opts.GitBranches == nil {
		return
	}
	call := func(w http.ResponseWriter, r *http.Request, fn func(context.Context) (gitworkspace.Result, error)) {
		result, err := fn(r.Context())
		if err == nil {
			writeJSON(w, r, http.StatusOK, result)
		} else {
			writeGitBranchError(w, r, err)
		}
	}
	mux.HandleFunc("POST /v1/workspaces/git/branches/list", func(w http.ResponseWriter, r *http.Request) {
		var req gitBranchListRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeGitBranchError(w, r, &gitworkspace.Error{Code: gitworkspace.CodeWorkspaceNotFound, Message: "invalid JSON body", Cause: err})
			return
		}
		call(w, r, func(ctx context.Context) (gitworkspace.Result, error) {
			return opts.GitBranches.List(ctx, req.WorkspacePath)
		})
	})
	mux.HandleFunc("POST /v1/workspaces/git/branches/create", func(w http.ResponseWriter, r *http.Request) {
		var req gitBranchCreateRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeGitBranchError(w, r, &gitworkspace.Error{Code: gitworkspace.CodeInvalidRef, Message: "invalid JSON body", Cause: err})
			return
		}
		start := ""
		if req.StartPoint != nil {
			start = *req.StartPoint
		}
		call(w, r, func(ctx context.Context) (gitworkspace.Result, error) {
			return opts.GitBranches.Create(ctx, req.WorkspacePath, req.Name, start, req.Checkout, req.ClientRequestID)
		})
	})
	mux.HandleFunc("POST /v1/workspaces/git/branches/checkout", func(w http.ResponseWriter, r *http.Request) {
		var req gitBranchCheckoutRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeGitBranchError(w, r, &gitworkspace.Error{Code: gitworkspace.CodeBranchNotFound, Message: "invalid JSON body", Cause: err})
			return
		}
		call(w, r, func(ctx context.Context) (gitworkspace.Result, error) {
			return opts.GitBranches.Checkout(ctx, req.WorkspacePath, req.Name, req.AllowDirty)
		})
	})
}

func writeGitBranchError(w http.ResponseWriter, r *http.Request, err error) {
	var ge *gitworkspace.Error
	if !errors.As(err, &ge) {
		ge = &gitworkspace.Error{Code: gitworkspace.CodeUnsupported, Message: err.Error(), Cause: err}
	}
	status := http.StatusConflict
	switch ge.Code {
	case gitworkspace.CodeWorkspaceNotFound:
		status = http.StatusNotFound
	case gitworkspace.CodeWorkspaceNotAuthorized:
		status = http.StatusForbidden
	case gitworkspace.CodeNotGitRepository, gitworkspace.CodeInvalidRef:
		status = http.StatusBadRequest
	case gitworkspace.CodeUnsupported:
		status = http.StatusNotImplemented
	case gitworkspace.CodeBranchNotFound:
		status = http.StatusNotFound
	case gitworkspace.CodeCheckoutFailed, gitworkspace.CodeCreateFailed:
		status = http.StatusBadGateway
	}
	writeJSON(w, r, status, ge)
}
