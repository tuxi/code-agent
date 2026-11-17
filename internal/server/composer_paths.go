package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"code-agent/internal/conversation"
)

func registerComposerPathValidationRoute(mux *http.ServeMux, repo conversation.ConversationRepository, opts MuxOptions) {
	mux.HandleFunc("POST /v1/composer/paths/validate", func(w http.ResponseWriter, r *http.Request) {
		if !managementAuthReady(w, r, opts.ServerAuth) {
			return
		}
		var req struct {
			composerQuery
			Paths []string `json:"paths"`
		}
		if decodeBoundedJSON(w, r, &req, 64<<10) != nil || len(req.Paths) > 32 {
			ErrorSimple(w, r, http.StatusBadRequest, "invalid reference paths")
			return
		}
		root, err := composerWorkspace(r.Context(), repo, opts, req.composerQuery)
		if err != nil || root == "" {
			ErrorSimple(w, r, http.StatusForbidden, "workspace scope unavailable")
			return
		}
		for _, path := range req.Paths {
			if r.Context().Err() != nil {
				return
			}
			if path == "" || strings.ContainsRune(path, 0) || len(path) > 4096 {
				ErrorSimple(w, r, http.StatusBadRequest, "invalid reference path")
				return
			}
			absolute := filepath.IsAbs(path)
			target := path
			if !absolute {
				clean := filepath.Clean(path)
				if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
					ErrorSimple(w, r, http.StatusForbidden, "reference escapes workspace")
					return
				}
				target = filepath.Join(root, clean)
			}
			permitsExternalMetadata := opts.RuntimeInfo.RuntimeProfile == RuntimeProfileFullDesktop || opts.RuntimeInfo.RuntimeProfile == RuntimeProfileHeadless
			if absolute && !permitsExternalMetadata {
				rel, err := filepath.Rel(root, filepath.Clean(target))
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					ErrorSimple(w, r, http.StatusForbidden, "external paths are unavailable in this runtime")
					return
				}
			}
			canonical, err := filepath.EvalSymlinks(target)
			if err != nil {
				ErrorSimple(w, r, http.StatusConflict, "a referenced path is missing or unreadable")
				return
			}
			rel, err := filepath.Rel(root, canonical)
			outside := err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
			if !absolute && outside {
				ErrorSimple(w, r, http.StatusForbidden, "reference escapes workspace")
				return
			}
			// Headless describes deployment, not filesystem authorization.
			// This endpoint checks metadata only; actual external reads still
			// pass through the tools' PathAccessApprover and protected paths.
			if outside && !permitsExternalMetadata {
				ErrorSimple(w, r, http.StatusForbidden, "external paths are unavailable in this runtime")
				return
			}
			if _, err := os.Stat(canonical); err != nil {
				ErrorSimple(w, r, http.StatusConflict, "a referenced path is missing or unreadable")
				return
			}
		}
		writeJSON(w, r, http.StatusOK, map[string]bool{"valid": true})
	})
}
