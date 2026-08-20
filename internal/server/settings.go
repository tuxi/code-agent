package server

import "net/http"

// registerSettingsRoutes exposes the explicit settings hot-reload boundary.
// The host owns persistence and validation; the server only reports whether
// the new snapshot was accepted by the live runtime.
func registerSettingsRoutes(mux *http.ServeMux, opts MuxOptions) {
	if opts.ReloadSettings == nil {
		return
	}
	mux.HandleFunc("POST /v1/settings/reload", func(w http.ResponseWriter, r *http.Request) {
		if err := opts.ReloadSettings(); err != nil {
			writeJSON(w, r, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"reloaded": true})
	})
}
