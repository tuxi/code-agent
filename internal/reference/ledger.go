// Package reference implements the Code-Agent side of the cross-MCP Reference
// Ledger Protocol. MCP servers keep returning their original opaque values;
// this package only creates and resolves session-local handles for the model.
package reference

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const handlePrefix = "$ref:"

// Descriptor is the optional top-level `references` entry in a tool's raw
// structuredContent. Pointer follows RFC 6901 and identifies one opaque value.
type Descriptor struct {
	Pointer   string `json:"pointer"`
	Kind      string `json:"kind"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

// InputRule is the optional x-codeagent-reference-inputs schema extension.
// Hosts that do not recognise it simply retain standard MCP behaviour.
type InputRule struct {
	Pointer string `json:"pointer"`
	Kind    string `json:"kind"`
	Mode    string `json:"mode"`
}

// Entry is persisted with the session. Value is deliberately never rendered in
// a model-facing message.
type Entry struct {
	Handle         string    `json:"handle"`
	RawValue       string    `json:"raw_value"`
	Kind           string    `json:"kind"`
	ProducerCallID string    `json:"producer_call_id"`
	SessionID      string    `json:"session_id"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	Scope          string    `json:"scope"`
}

// Error is a stable error returned before a malformed reference reaches MCP.
type Error struct {
	Code      string       `json:"code"`
	Handle    string       `json:"handle,omitempty"`
	Pointer   string       `json:"pointer,omitempty"`
	Kind      string       `json:"kind,omitempty"`
	Detail    string       `json:"message,omitempty"`
	Available []HandleHint `json:"available,omitempty"`
}

// HandleHint is intentionally safe for a model-facing recovery error: it
// contains a session-local handle and metadata, never the original opaque MCP
// value. It lets a model recover from a guessed ID without another blind call.
type HandleHint struct {
	Handle    string `json:"handle"`
	Kind      string `json:"kind"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

func (e *Error) Error() string {
	b, err := json.Marshal(e)
	if err != nil {
		return e.Code
	}
	return string(b)
}

// Register reads descriptors from raw structuredContent and appends ledger
// entries. It returns substitutions for redacting the model-facing observation.
// Invalid optional metadata is ignored: it must never make a normal MCP tool
// result fail merely because a host supplied an extension incorrectly.
func Register(raw json.RawMessage, existing []Entry, sessionID, callID string, now time.Time) ([]Entry, map[string]string) {
	var document any
	if len(raw) == 0 || json.Unmarshal(raw, &document) != nil {
		return existing, nil
	}
	root, ok := document.(map[string]any)
	if !ok {
		return existing, nil
	}
	b, err := json.Marshal(root["references"])
	if err != nil || len(b) == 0 || string(b) == "null" {
		return existing, nil
	}
	var descriptors []Descriptor
	if json.Unmarshal(b, &descriptors) != nil {
		return existing, nil
	}

	entries := append([]Entry(nil), existing...)
	substitutions := make(map[string]string)
	next := nextHandle(entries)
	for _, descriptor := range descriptors {
		if descriptor.Pointer == "" || descriptor.Kind == "" {
			continue
		}
		value, ok := getString(document, descriptor.Pointer)
		if !ok || value == "" {
			continue
		}
		expiresAt, ok := parseExpiry(descriptor.ExpiresAt)
		if !ok {
			// The descriptor already marked this field opaque. Preserve that
			// confidentiality boundary even when its optional expiry metadata is
			// malformed; a missing TTL is represented by the zero time.
			expiresAt = time.Time{}
		}
		handle := fmt.Sprintf("ref_%04d", next)
		next++
		entries = append(entries, Entry{
			Handle: handle, RawValue: value, Kind: descriptor.Kind,
			ProducerCallID: callID, SessionID: sessionID, CreatedAt: now,
			ExpiresAt: expiresAt, Scope: scopeOrSession(descriptor.Scope),
		})
		substitutions[value] = handlePrefix + handle
	}
	if len(substitutions) == 0 {
		return existing, nil
	}
	return entries, substitutions
}

// ResolveInput validates required handle fields and recursively resolves every
// $ref:<handle> value into the original MCP argument immediately before call.
func ResolveInput(raw, schema json.RawMessage, entries []Entry, sessionID string, now time.Time) (json.RawMessage, error) {
	var value any
	if len(raw) == 0 {
		value = map[string]any{}
	} else if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	rules := inputRules(schema)
	for _, rule := range rules {
		if rule.Mode != "handle_only" {
			continue
		}
		candidate, exists := get(value, rule.Pointer)
		if !exists {
			continue // ordinary JSON-schema required validation remains the tool's job.
		}
		s, ok := candidate.(string)
		if !ok || !strings.HasPrefix(s, handlePrefix) {
			return nil, referenceError(
				"reference_handle_required", "", rule.Pointer, rule.Kind,
				"field "+rule.Pointer+" requires a $ref handle", entries, sessionID, now,
			)
		}
		resolved, err := resolve(s, entries, sessionID, now, rule.Kind, rule.Pointer)
		if err != nil {
			return nil, err
		}
		if !set(value, rule.Pointer, resolved) {
			return nil, fmt.Errorf("invalid reference rule pointer: %s", rule.Pointer)
		}
	}
	resolved, err := resolve(value, entries, sessionID, now, "", "")
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(resolved)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func resolve(value any, entries []Entry, sessionID string, now time.Time, expectedKind, pointer string) (any, error) {
	switch v := value.(type) {
	case string:
		if !strings.HasPrefix(v, handlePrefix) {
			return v, nil
		}
		handle := strings.TrimPrefix(v, handlePrefix)
		for _, entry := range entries {
			if entry.Handle != handle {
				continue
			}
			// SessionID is the access-control boundary. Scope is retained as
			// resource semantics (for example an AX element scoped to a snapshot),
			// not as a cross-session permission check. A later parent-handle rule
			// can bind an element to its snapshot without making valid snapshot-
			// scoped references unusable here.
			if entry.SessionID != sessionID {
				return nil, &Error{Code: "reference_scope_denied", Handle: handle}
			}
			if !entry.ExpiresAt.IsZero() && !now.Before(entry.ExpiresAt) {
				return nil, &Error{Code: "reference_expired", Handle: handle}
			}
			if expectedKind != "" && entry.Kind != expectedKind {
				return nil, referenceError(
					"reference_kind_mismatch", handle, pointer, expectedKind,
					"expected "+expectedKind+", got "+entry.Kind, entries, sessionID, now,
				)
			}
			return entry.RawValue, nil
		}
		return nil, referenceError("reference_not_found", handle, pointer, expectedKind, "", entries, sessionID, now)
	case map[string]any:
		for key, child := range v {
			resolved, err := resolve(child, entries, sessionID, now, "", "")
			if err != nil {
				return nil, err
			}
			v[key] = resolved
		}
		return v, nil
	case []any:
		for i, child := range v {
			resolved, err := resolve(child, entries, sessionID, now, "", "")
			if err != nil {
				return nil, err
			}
			v[i] = resolved
		}
		return v, nil
	default:
		return value, nil
	}
}

func referenceError(code, handle, pointer, kind, detail string, entries []Entry, sessionID string, now time.Time) *Error {
	hints := availableHandles(entries, sessionID, now, kind)
	if kind != "" && len(hints) == 0 && detail != "" {
		detail += "; no active " + kind + " handles are available; obtain one from a prior tool result"
	}
	return &Error{
		Code: code, Handle: handle, Pointer: pointer, Kind: kind, Detail: detail,
		Available: hints,
	}
}

func availableHandles(entries []Entry, sessionID string, now time.Time, kind string) []HandleHint {
	hints := make([]HandleHint, 0)
	for _, entry := range entries {
		if entry.SessionID != sessionID || (!entry.ExpiresAt.IsZero() && !now.Before(entry.ExpiresAt)) {
			continue
		}
		if kind != "" && entry.Kind != kind {
			continue
		}
		hint := HandleHint{Handle: handlePrefix + entry.Handle, Kind: entry.Kind}
		if !entry.ExpiresAt.IsZero() {
			hint.ExpiresAt = entry.ExpiresAt.UTC().Format(time.RFC3339)
		}
		hints = append(hints, hint)
	}
	return hints
}

func inputRules(schema json.RawMessage) []InputRule {
	var root map[string]json.RawMessage
	if len(schema) == 0 || json.Unmarshal(schema, &root) != nil {
		return nil
	}
	b := root["x-codeagent-reference-inputs"]
	var rules []InputRule
	_ = json.Unmarshal(b, &rules)
	return rules
}

func parseExpiry(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

func scopeOrSession(scope string) string {
	if scope == "" {
		return "session"
	}
	return scope
}

func nextHandle(entries []Entry) int { return len(entries) + 1 }

func getString(document any, pointer string) (string, bool) {
	v, ok := get(document, pointer)
	s, stringOK := v.(string)
	return s, ok && stringOK
}

func get(document any, pointer string) (any, bool) {
	if pointer == "" {
		return document, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	current := document
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[token]
			if !ok {
				return nil, false
			}
		case []any:
			var index int
			if _, err := fmt.Sscanf(token, "%d", &index); err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func set(document any, pointer string, replacement any) bool {
	if pointer == "" || !strings.HasPrefix(pointer, "/") {
		return false
	}
	tokens := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	current := document
	for i, token := range tokens {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		last := i == len(tokens)-1
		switch node := current.(type) {
		case map[string]any:
			if last {
				if _, ok := node[token]; !ok {
					return false
				}
				node[token] = replacement
				return true
			}
			var ok bool
			current, ok = node[token]
			if !ok {
				return false
			}
		case []any:
			var index int
			if _, err := fmt.Sscanf(token, "%d", &index); err != nil || index < 0 || index >= len(node) {
				return false
			}
			if last {
				node[index] = replacement
				return true
			}
			current = node[index]
		default:
			return false
		}
	}
	return false
}
