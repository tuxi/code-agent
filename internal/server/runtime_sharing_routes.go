package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

func registerRuntimeSharingRoutes(mux *http.ServeMux, sharing *DaemonRuntimeSharing, info RuntimeInfo) {
	managementOnly := func(w http.ResponseWriter, r *http.Request) bool {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !isLoopbackListenHost(host) {
			Error(w, r, http.StatusForbidden, 40300, "sharing management is localhost-only")
			return false
		}
		return true
	}
	mux.HandleFunc("POST /v1/runtime/sharing/start", func(w http.ResponseWriter, r *http.Request) {
		if !managementOnly(w, r) {
			return
		}
		var req struct {
			DisplayName   string `json:"display_name"`
			ListenAddress string `json:"listen_address"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				Error(w, r, http.StatusBadRequest, CodeBadRequest, "invalid sharing start request")
				return
			}
		}
		if err := sharing.Start(req.ListenAddress, req.DisplayName); err != nil {
			Error(w, r, http.StatusInternalServerError, CodeInternal, "could not start sharing")
			return
		}
		Success(w, r, sharing.Status())
	})
	mux.HandleFunc("POST /v1/runtime/sharing/stop", func(w http.ResponseWriter, r *http.Request) {
		if !managementOnly(w, r) {
			return
		}
		if err := sharing.Stop(); err != nil {
			Error(w, r, http.StatusInternalServerError, CodeInternal, "could not stop sharing")
			return
		}
		Success(w, r, nil)
	})
	mux.HandleFunc("GET /v1/runtime/sharing/status", func(w http.ResponseWriter, r *http.Request) {
		if managementOnly(w, r) {
			Success(w, r, sharing.Status())
		}
	})
	mux.HandleFunc("POST /v1/runtime/sharing/invitations", func(w http.ResponseWriter, r *http.Request) {
		if !managementOnly(w, r) {
			return
		}
		var req struct {
			ValiditySeconds int `json:"validity_seconds"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		inv, err := sharing.CreateInvitation(req.ValiditySeconds)
		if err != nil {
			Error(w, r, http.StatusConflict, 40900, "could not create pairing invitation")
			return
		}
		Success(w, r, inv)
	})
	mux.HandleFunc("GET /v1/runtime/sharing/devices", func(w http.ResponseWriter, r *http.Request) {
		if !managementOnly(w, r) {
			return
		}
		Success(w, r, map[string]any{"devices": sharing.Devices()})
	})
	mux.HandleFunc("DELETE /v1/runtime/sharing/devices/{device_id}", func(w http.ResponseWriter, r *http.Request) {
		if !managementOnly(w, r) {
			return
		}
		id := strings.TrimSpace(r.PathValue("device_id"))
		if id == "" {
			Error(w, r, http.StatusBadRequest, CodeBadRequest, "device_id is required")
			return
		}
		if err := sharing.Revoke(id); err != nil {
			Error(w, r, http.StatusNotFound, CodeNotFound, "shared device not found")
			return
		}
		Success(w, r, nil)
	})
	_ = info
}
