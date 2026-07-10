package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// apiResponse is the unified envelope for all HTTP responses, matching the
// Agent Gateway format so a single client decoder works for both services.
type apiResponse struct {
	TraceID string `json:"trace_id"`
	Code    int    `json:"code"`
	Msg     string `json:"msg"`
	Data    any    `json:"data,omitempty"`
}

// traceCtxKey is the context key for a per-request trace ID.
type traceCtxKey struct{}

// WithTraceID attaches a trace ID to the request context. Call in middleware.
func WithTraceID(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), traceCtxKey{}, id))
}

func traceID(r *http.Request) string {
	if r != nil {
		if id, ok := r.Context().Value(traceCtxKey{}).(string); ok && id != "" {
			return id
		}
	}
	return uuid.New().String()
}

// Success writes a 200 response with code=0.
func Success(w http.ResponseWriter, r *http.Request, data any) {
	writeResponse(w, http.StatusOK, apiResponse{
		TraceID: traceID(r),
		Code:    0,
		Msg:     "success",
		Data:    data,
	})
}

// Created writes a 201 response with code=0.
func Created(w http.ResponseWriter, r *http.Request, data any) {
	writeResponse(w, http.StatusCreated, apiResponse{
		TraceID: traceID(r),
		Code:    0,
		Msg:     "success",
		Data:    data,
	})
}

// NoContent writes a 204 with no body.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Error writes an error response. status is the HTTP status code; code is the
// business error code. msg is a human-readable description.
func Error(w http.ResponseWriter, r *http.Request, status int, code int, msg string) {
	writeResponse(w, status, apiResponse{
		TraceID: traceID(r),
		Code:    code,
		Msg:     msg,
	})
}

// ErrorSimple is like Error but derives business code from HTTP status.
func ErrorSimple(w http.ResponseWriter, r *http.Request, status int, msg string) {
	code := status * 100 // 400→40000, 404→40400, 500→50000
	writeResponse(w, status, apiResponse{
		TraceID: traceID(r),
		Code:    code,
		Msg:     msg,
	})
}

// Result writes a response with an explicit HTTP status, business code, and
// optional data payload. Used for structured error responses like quota_exceeded.
func Result(w http.ResponseWriter, r *http.Request, status int, code int, msg string, data any) {
	writeResponse(w, status, apiResponse{
		TraceID: traceID(r),
		Code:    code,
		Msg:     msg,
		Data:    data,
	})
}

// Common business error codes, matching Agent Gateway conventions.
const (
	CodeBadRequest   = 40000
	CodeNotFound     = 40400
	CodeInternal     = 50000
	CodeUnauthorized = 40100
)

func writeResponse(w http.ResponseWriter, status int, resp apiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
