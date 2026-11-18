package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

const textSupplementCapability = "user_supplement_text_v1"

type supplementService interface {
	SupportsTextSupplements() bool
	SubmitTextSupplement(context.Context, session.Supplement) (session.SupplementReceipt, bool, error)
	QuerySupplement(context.Context, string, string) (session.SupplementReceipt, error)
}

// Admission is text-only until ownership validation for staged media has been
// added. A separate capability avoids implying support for media or queueing.
func registerSupplementRoutes(mux *http.ServeMux, service supplementService, enabled bool) bool {
	available := enabled && service != nil && service.SupportsTextSupplements()
	respond := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	failure := func(w http.ResponseWriter, err error) {
		status, code := http.StatusInternalServerError, "supplement_storage_error"
		switch {
		case errors.Is(err, conversation.ErrSupplementUnavailable):
			status, code = http.StatusNotImplemented, "supplement_unsupported"
		case errors.Is(err, session.ErrInvalidSupplement):
			status, code = http.StatusBadRequest, "invalid_supplement"
		case errors.Is(err, session.ErrSupplementConflict):
			status, code = http.StatusConflict, "request_conflict"
		case errors.Is(err, session.ErrSupplementTargetClosed):
			status, code = http.StatusConflict, "target_turn_closed"
		case errors.Is(err, sql.ErrNoRows):
			status, code = http.StatusNotFound, "supplement_not_found"
		}
		respond(w, status, map[string]string{"code": code})
	}
	mux.HandleFunc("POST /v1/conversations/{id}/supplements", func(w http.ResponseWriter, r *http.Request) {
		if !available {
			failure(w, conversation.ErrSupplementUnavailable)
			return
		}
		var body struct {
			RequestID      string `json:"request_id"`
			ExpectedTurnID string `json:"expected_turn_id"`
			Text           string `json:"text"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			failure(w, session.ErrInvalidSupplement)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			failure(w, session.ErrInvalidSupplement)
			return
		}
		receipt, _, err := service.SubmitTextSupplement(r.Context(), session.Supplement{SessionID: r.PathValue("id"), RequestID: body.RequestID, ExpectedTurnID: body.ExpectedTurnID, Text: body.Text})
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, http.StatusAccepted, receipt)
	})
	mux.HandleFunc("GET /v1/conversations/{id}/supplements/{request_id}", func(w http.ResponseWriter, r *http.Request) {
		if !available {
			failure(w, conversation.ErrSupplementUnavailable)
			return
		}
		receipt, err := service.QuerySupplement(r.Context(), r.PathValue("id"), r.PathValue("request_id"))
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, http.StatusOK, receipt)
	})
	return available
}
