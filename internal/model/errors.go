package model

import "fmt"

// APIError is a non-2xx response from a model provider. It carries the HTTP
// status so the resilience layer can classify the failure: 408/429/5xx are
// transient and retryable, other 4xx (bad request, auth, context-too-large) are
// not. Provider implementations should return *APIError for non-2xx responses.
type APIError struct {
	StatusCode int
	Type       string
	Message    string
	Body       string
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
