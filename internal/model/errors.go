package model

import (
	"errors"
	"fmt"
)

var userAssetErrorMessages = map[string]string{
	"asset_unavailable":        "One or more image assets are unavailable",
	"asset_not_ready":          "One or more image assets are not ready",
	"invalid_assets":           "One or more image assets are invalid",
	"asset_integrity_mismatch": "One or more image assets failed integrity validation",
	"image_input_unsupported":  "The selected model cannot process image input",
	"image_processing_failed":  "Image processing failed",
}

func UserAssetErrorCode(err error) (string, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Type != "user_asset_error" {
		return "", false
	}
	if _, known := userAssetErrorMessages[apiErr.Code]; known {
		return apiErr.Code, true
	}
	// The Gateway contract is extensible, but unknown asset failures must not
	// leak their upstream message or accidentally enter provider fallback.
	return "request_failed", true
}

func SafeUserAssetErrorMessage(code string) string {
	if message := userAssetErrorMessages[code]; message != "" {
		return message
	}
	return "Request failed"
}

// APIError is a non-2xx response from a model provider. It carries the HTTP
// status so the resilience layer can classify the failure: 408/429/5xx are
// transient and retryable, other 4xx (bad request, auth, context-too-large) are
// not. Provider implementations should return *APIError for non-2xx responses.
type APIError struct {
	StatusCode int
	Type       string
	// Code is the provider's stable, machine-readable error classification.
	// Gateway uses quota_exceeded for a user's exhausted allowance; it is not
	// equivalent to a transient upstream HTTP 429.
	Code    string
	Message string
	Body    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("model api error: status=%d type=%s message=%s", e.StatusCode, e.Type, e.Message)
	}
	body := e.Body
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	return fmt.Sprintf("model api error: status=%d body=%s", e.StatusCode, body)
}
