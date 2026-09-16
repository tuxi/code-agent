package model

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	// CredentialTarget identifies the connection-scoped credential involved in
	// an authentication failure. It is safe metadata such as
	// "llm/company-production"; it never contains the resolved secret.
	CredentialTarget string
	// Code is the provider's stable, machine-readable error classification.
	// Gateway uses quota_exceeded for a user's exhausted allowance; it is not
	// equivalent to a transient upstream HTTP 429.
	Code    string
	Message string
	Body    string
}

// authFailureHints are the substrings that mark a 401/403 as an authentication
// failure. Only such errors are reported as "provider authentication failed"
// and mapped to the host's auth_expired refresh contract; a structural 403
// (region lock, entitlement, model-not-entitled) carries a different upstream
// type/message and must be surfaced as-is.
var authFailureHints = []string{
	"auth", "unauthor", "api key", "apikey", "invalid_api_key",
	"invalid key", "credential", "permission",
}

// IsAuthFailure reports whether a non-2xx APIError is an authentication failure
// rather than a structural rejection. A 401 is always authentication. A 403 is
// authentication when the upstream error text carries an auth hint, or when the
// provider supplied no detail at all (the historical default for a bare 403).
func IsAuthFailure(e *APIError) bool {
	if e == nil {
		return false
	}
	if e.StatusCode == http.StatusUnauthorized {
		return true
	}
	if e.StatusCode != http.StatusForbidden {
		return false
	}
	hay := strings.ToLower(e.Type + " " + e.Code + " " + e.Message)
	for _, hint := range authFailureHints {
		if strings.Contains(hay, hint) {
			return true
		}
	}
	// No structured upstream detail: keep classifying as auth so a bare 403
	// behaves exactly as before.
	return strings.TrimSpace(e.Type+e.Code+e.Message) == ""
}

// redactSecret removes the resolved credential value from a provider message
// before it is surfaced. Relays occasionally echo the Authorization header in
// an error message; the raw body is always dropped, and this scrubs the decoded
// message as well when the secret is known.
func redactSecret(message, secret string) string {
	if secret == "" || message == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[redacted]")
}

func (e *APIError) Error() string {
	target := ""
	if e.CredentialTarget != "" {
		target = fmt.Sprintf(" credential_target=%q", e.CredentialTarget)
	}
	if e.Message != "" {
		return fmt.Sprintf("model api error: status=%d%s type=%s message=%s", e.StatusCode, target, e.Type, e.Message)
	}
	body := e.Body
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	return fmt.Sprintf("model api error: status=%d%s body=%s", e.StatusCode, target, body)
}
