package mcpbridge

import (
	"encoding/json"
	"fmt"
	"io"

	"code-agent/internal/collab"
	"github.com/google/uuid"
)

// RuntimeAPIError is safe to serialize into an MCP isError text block. Runtime
// messages, response bodies and unknown codes are never reflected to the model.
type RuntimeAPIError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status"`
	TraceID    string `json:"trace_id,omitempty"`
}

func (e *RuntimeAPIError) Error() string {
	b, _ := json.Marshal(struct {
		Error *RuntimeAPIError `json:"error"`
	}{e})
	return string(b)
}

type safeRuntimeError struct {
	status  int
	message string
	cause   error
}

var safeRuntimeErrors = map[string]safeRuntimeError{
	"recovery_required":          {409, "Execution requires recovery reconciliation; do not resubmit or replay side effects.", collab.ErrRecoveryRequired},
	"managed_workspace":          {409, "Workspace is owned by managed execution.", collab.ErrManagedWorkspace},
	"executor_model_unavailable": {409, "Selected executor model is unavailable; no fallback was used.", collab.ErrExecutorModelUnavailable},
	"task_conflict":              {409, "Task request conflicts with persisted data; inspect the original task before retrying.", collab.ErrConflict},
	"invalid_task_request":       {400, "Invalid task request.", collab.ErrInvalid},
	"task_resource_not_found":    {404, "Task resource was not found.", collab.ErrNotFound},
	"task_forbidden":             {403, "Task operation is not permitted.", collab.ErrForbidden},
	"managed_unavailable":        {503, "Managed task capability is unavailable.", collab.ErrUnavailable},
}

func (e *RuntimeAPIError) Unwrap() error { return safeRuntimeErrors[e.Code].cause }

func readRuntimeAPIError(status int, body io.Reader) error {
	out := &RuntimeAPIError{Code: "runtime_http_error", Message: fmt.Sprintf("Runtime request failed with HTTP %d.", status), HTTPStatus: status}
	b, err := io.ReadAll(io.LimitReader(body, (16<<10)+1))
	if err != nil || len(b) > 16<<10 {
		return out
	}
	var response struct {
		Code    json.RawMessage `json:"code"`
		TraceID string          `json:"trace_id"`
		Data    struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &response) != nil {
		return out
	}
	code := response.Data.Code
	if code == "" {
		_ = json.Unmarshal(response.Code, &code)
	} // legacy bare task error
	if safe, ok := safeRuntimeErrors[code]; ok && safe.status == status {
		out.Code, out.Message = code, safe.message
	}
	if id, err := uuid.Parse(response.TraceID); err == nil && id.String() == response.TraceID && id != uuid.Nil {
		out.TraceID = response.TraceID
	}
	return out
}
