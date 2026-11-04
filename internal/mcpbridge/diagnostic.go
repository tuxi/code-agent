package mcpbridge

import "encoding/json"

// BridgeError is safe for the SDK's standard isError text content and stderr.
// Causes remain available to Go callers but are never reflected to the client.
type BridgeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
	NextAction string `json:"next_action"`
	SessionID  string `json:"session_id,omitempty"`
	TurnID     string `json:"turn_id,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	cause      error
}

func (e *BridgeError) Error() string {
	b, _ := json.Marshal(struct {
		Error *BridgeError `json:"error"`
	}{e})
	return string(b)
}

func (e *BridgeError) Unwrap() error { return e.cause }

type diagnosticText struct{ message, action string }

var bridgeDiagnostics = map[string]diagnosticText{
	"invalid_validation_contract":     {"The validation contract is invalid.", "Correct the declared checks before submitting; never change an admitted request's payload."},
	"validation_contract_unsupported": {"Durable validation contracts are unavailable or could not be verified; input was not sent.", "Use a Runtime with durable_validation_contract_v1. Do not silently fall back to process-local expectations."},
	"binding_not_found":               {"The explicit binding is missing or retired.", "Bind the exact original Session, baseline, Runtime and Store. Never fall back to a current or latest Session."},
	"binding_limit":                   {"This Bridge has reached its in-memory binding limit.", "Do not discard unresolved requests. Finish or reconcile existing work before intentionally restarting and explicitly reattaching."},
	"grant_invalid":                   {"Local MCP grant is missing, unsafe, or invalid.", "Check the private grant path and permissions; authorize local MCP access in Talkify. Never paste credentials into chat."},
	"grant_expired":                   {"Local MCP grant has expired.", "Authorize local MCP access in Talkify, then reconnect and verify the same Runtime, Store and workspace. Do not resubmit uncertain work."},
	"grant_replaced":                  {"Local MCP grant was revoked or replaced.", "After user authorization, use Manual codeagent_connection_info reconnect with the original server_id, store_id and workspace_path; then explicitly replace existing Session bindings. Legacy profiles require reconnecting MCP. Never follow a replacement grant automatically or resubmit uncertain work."},
	"connection_closed":               {"This Bridge connection is closed.", "Reconnect and verify identities before reading the original Turn. Reconnection does not replay execution or restore approvals."},
	"daemon_stopped":                  {"The Daemon process recorded in the local grant is no longer available.", "Check Talkify's Daemon and authorize its current instance, then reconnect. Do not restart or replay work automatically."},
	"runtime_unreachable":             {"The Runtime connection failed.", "Check Talkify and the local connection, then verify identity. If a send receipt is missing, look up its original request_id before any retry."},
	"identity_mismatch":               {"Runtime or Store identity does not match the authorized binding.", "Stop delegation and verify the intended Runtime and Store. Do not retarget automatically."},
	"workspace_mismatch":              {"Workspace or Git identity does not match the execution binding.", "Check the intended repository and Session. Do not redirect the binding or delete user files."},
	"baseline_changed":                {"Workspace HEAD differs from the original execution baseline.", "Stop new execution on this binding. Preserve the original baseline and review delivered work; use a new Session and baseline for a new task."},
	"session_mismatch":                {"Session is not owned by this Bridge or does not match its workspace policy.", "Verify the explicit Session and workspace. Do not silently create or select another Session."},
	"session_busy":                    {"The Session has active work or pending human controls.", "Observe the original Turn in Talkify and let the user handle approvals. Do not submit another Turn or approve automatically."},
	"turn_not_terminal":               {"The Turn has not reached a terminal state; final artifacts are unavailable.", "Read this exact Turn for its state and attention references. Let the user handle approvals in Talkify; do not treat waiting as completion."},
	"admission_unknown":               {"Execution admission is uncertain; work may already have been submitted.", "Use codeagent_lookup_admission with the original Session and request_id. Never allocate a new request_id; unknown is not unsubmitted. After restart reconcile durable history; do not blindly retry."},
	"create_unknown":                  {"Session creation outcome is uncertain.", "Inspect the Daemon's Session list before retrying creation. Do not assume a lost response means no Session was created."},
	"request_conflict":                {"The request_id was used with a different payload.", "Do not retry or change the existing request. Inspect its original admission before deciding whether there is a separate new task."},
	"already_admitted":                {"The request_id was admitted before reattachment.", "Read the original admission and exact Turn; do not resubmit it."},
	"evidence_incomplete":             {"Complete, unambiguous durable execution evidence is unavailable.", "Read the original Turn and reconcile missing evidence. Do not infer completion or validation, or replay execution."},
	"invalid_tool_arguments":          {"Tool arguments are invalid.", "Correct the arguments without changing an existing admitted request's payload."},
}

func bridgeError(code string, cause error) *BridgeError {
	t, ok := bridgeDiagnostics[code]
	if !ok {
		panic("unregistered Bridge diagnostic code")
	}
	return &BridgeError{Code: code, Message: t.message, NextAction: t.action, cause: cause}
}

func admissionError(code, session, request string, cause error) *BridgeError {
	e := bridgeError(code, cause)
	if validID(session) {
		e.SessionID = session
	}
	if validID(request) {
		e.RequestID = request
	}
	return e
}
