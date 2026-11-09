package server

import (
	"errors"
	"net/http"
	"strconv"

	"code-agent/internal/conversation"
	"code-agent/internal/preview"
)

func registerPreviewRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, service *preview.Service) {
	mux.HandleFunc("GET /v1/conversations/{id}/previews/{artifact_id}/revisions/{revision}/viewer", func(w http.ResponseWriter, r *http.Request) {
		if service == nil || !service.Available() {
			writePreviewError(w, r, preview.NewError(preview.CodeStoreUnavailable, "Preview store is unavailable.", true))
			return
		}
		revision, ok := previewRevision(w, r)
		if !ok {
			return
		}
		if _, err := repo.Load(r.Context(), r.PathValue("id")); err != nil {
			writePreviewError(w, r, preview.NewError(preview.CodeArtifactMissing, "Preview artifact was not found.", false))
			return
		}
		resource, err := service.ResolveViewer(r.Context(), r.PathValue("id"), r.PathValue("artifact_id"), revision)
		if err != nil {
			writePreviewError(w, r, err)
			return
		}
		writePreviewResource(w, resource, true)
	})
	mux.HandleFunc("GET /v1/conversations/{id}/previews/{artifact_id}/revisions/{revision}/assets/{asset_id}/content", func(w http.ResponseWriter, r *http.Request) {
		if service == nil || !service.Available() {
			writePreviewError(w, r, preview.NewError(preview.CodeStoreUnavailable, "Preview store is unavailable.", true))
			return
		}
		revision, ok := previewRevision(w, r)
		if !ok {
			return
		}
		if _, err := repo.Load(r.Context(), r.PathValue("id")); err != nil {
			writePreviewError(w, r, preview.NewError(preview.CodeAssetUnavailable, "Preview asset was not found.", false))
			return
		}
		resource, err := service.ResolveAsset(r.Context(), r.PathValue("id"), r.PathValue("artifact_id"), revision, r.PathValue("asset_id"))
		if err != nil {
			writePreviewError(w, r, err)
			return
		}
		writePreviewResource(w, resource, false)
	})
}

func previewRevision(w http.ResponseWriter, r *http.Request) (int64, bool) {
	revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || revision <= 0 {
		writePreviewError(w, r, preview.NewError(preview.CodeInvalidArtifact, "Preview revision is invalid.", false))
		return 0, false
	}
	return revision, true
}

func writePreviewResource(w http.ResponseWriter, resource preview.Resource, viewer bool) {
	w.Header().Set("Content-Type", resource.MIMEType)
	w.Header().Set("Content-Length", strconv.FormatInt(resource.SizeBytes, 10))
	w.Header().Set("ETag", `"`+resource.SHA256+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if viewer {
		w.Header().Set("Content-Security-Policy", preview.ViewerContentSecurityPolicy)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resource.Data)
}

func writePreviewError(w http.ResponseWriter, r *http.Request, err error) {
	pe := preview.NewError(preview.CodeInvalidArtifact, "Preview request failed.", false)
	var target *preview.Error
	if errors.As(err, &target) {
		pe = target
	}
	if pe.HTTP == 0 {
		pe.HTTP = http.StatusUnprocessableEntity
	}
	data := map[string]any{
		"http_status": pe.HTTP,
		"error": map[string]any{
			"code":      pe.Code,
			"message":   pe.Message,
			"retryable": pe.Retryable,
		},
	}
	if pe.Code == preview.CodeUnsupported {
		data["presentation"] = "unavailable"
		Result(w, r, http.StatusOK, 0, "success", data)
		return
	}
	Result(w, r, pe.HTTP, pe.HTTP*100, string(pe.Code), data)
}
