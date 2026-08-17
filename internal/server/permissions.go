package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// PermissionService manages the workspace approval tier (ask/auto/full). The
// server layer is a dumb pipe: the interface is implemented by the assembler
// (embed.Assemble / cmd serve) which owns the settings paths. A nil Permissions
// disables the endpoints (404), matching the Granter/Providers pattern.
//
// The tier is a single per-workspace scalar, distinct from
// permissions.allow/deny — those remain owned by the approval card's "always
// allow" grants and are intentionally NOT exposed here.
type PermissionService interface {
	// EffectiveMode returns the tier in effect for the workspace: the
	// workspace's settings.local.json, falling back through the settings merge
	// (user → shared → local), defaulting to "ask".
	EffectiveMode(root string) (string, error)
	// SetMode persists the tier to the workspace's settings.local.json
	// (per-workspace, machine-local — never the shared file). Valid modes are
	// "ask", "auto", "full".
	SetMode(root, mode string) error
}

// permissionResponse is the GET /v1/workspaces/{path}/permissions payload.
// available lists the three selectable tiers; mode is the effective one.
// scope is always "workspace" in v1 — a user-global tier endpoint
// (/v1/permissions, scope=user) is a future extension and will reuse this shape.
type permissionResponse struct {
	Scope     string   `json:"scope"`
	Path      string   `json:"path"`
	Available []string `json:"available"`
	Mode      string   `json:"mode"`
}

// validApprovalMode reports whether mode is one of the three tiers.
func validApprovalMode(mode string) bool {
	switch mode {
	case "ask", "auto", "full":
		return true
	default:
		return false
	}
}

// normalizeWorkspacePath restores the leading slash the {path...} wildcard
// strips from an absolute path ("tmp/foo" → "/tmp/foo"). Relative paths are
// passed through unchanged.
func normalizeWorkspacePath(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// registerPermissionRoutes wires GET/PUT /v1/workspaces/permissions/{path...}
// onto mux. A nil service leaves the endpoints absent (404), matching other
// optional MuxOptions. {path...} is a trailing multi-segment wildcard so an
// absolute workspace path survives URL decoding (Go's ServeMux decodes %2F
// before matching, and a wildcard cannot be followed by a suffix — the
// /mcp/reload single-segment {path} cannot carry an absolute path either).
func registerPermissionRoutes(mux *http.ServeMux, opts MuxOptions) {
	if opts.Permissions == nil {
		return
	}
	svc := opts.Permissions
	available := []string{"ask", "auto", "full"}

	mux.HandleFunc("GET /v1/workspaces/permissions/{path...}", func(w http.ResponseWriter, r *http.Request) {
		root := normalizeWorkspacePath(r.PathValue("path"))
		mode, err := svc.EffectiveMode(root)
		if err != nil {
			writeJSON(w, r, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, r, http.StatusOK, permissionResponse{
			Scope: "workspace", Path: root, Available: available, Mode: mode,
		})
	})

	mux.HandleFunc("PUT /v1/workspaces/permissions/{path...}", func(w http.ResponseWriter, r *http.Request) {
		root := normalizeWorkspacePath(r.PathValue("path"))
		if root == "" {
			writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": "workspace path required"})
			return
		}
		var body struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
			return
		}
		if !validApprovalMode(body.Mode) {
			writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": "mode must be one of ask, auto, full"})
			return
		}
		if err := svc.SetMode(root, body.Mode); err != nil {
			writeJSON(w, r, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"mode": body.Mode, "applied": true})
	})
}
