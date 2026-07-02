// Package server is Layer 2 of the agent: it maps core events (agent.Event) onto
// the agent-wire protocol and streams them to frontends. The core (internal/agent)
// never depends on this package — agent.Event keeps its rich Go types, and every
// "how it goes on the wire" decision (millisecond durations, RFC3339 timestamps,
// structured tool args) lives here. See docs/protocols/agent-wire-v1.md.
package server

import (
	"encoding/json"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
	"code-agent/internal/tools"
)

// wireEvent is the on-the-wire form of an agent.Event (protocol v1). It is the
// single place transport concerns are expressed; the core struct is left
// untouched. Field order groups the common header first, then per-kind fields.
type wireEvent struct {
	// Common header (§3.1). event_id and parent_session_id are stamped by the
	// emitter, not by toWire, which keeps toWire pure and golden-testable.
	EventID         string `json:"event_id,omitempty"`
	Kind            string `json:"kind"`
	At              string `json:"at"`
	SessionID       string `json:"session_id,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	TurnID          string `json:"turn_id,omitempty"`
	InvocationID    string `json:"invocation_id,omitempty"`
	// Seq is the per-session monotonic event sequence (v1.2 §4). A client records
	// the highest seq it has seen and, on reconnect, requests
	// GET …/events?since=<seq> to replay only the tail it missed. Live events carry
	// the same seq the replay path reports.
	Seq int64 `json:"seq,omitempty"`

	// Tool / skill events.
	CallID          string                  `json:"call_id,omitempty"`
	Step            int                     `json:"step,omitempty"`
	ToolName        string                  `json:"tool_name,omitempty"`
	ToolArgs        json.RawMessage         `json:"tool_args,omitempty"`
	Observation     string                  `json:"observation,omitempty"`
	Output          json.RawMessage         `json:"output,omitempty"`
	Assets          []assets.Ref            `json:"assets,omitempty"`
	TextAnnotations []assets.TextAnnotation `json:"text_annotations,omitempty"`
	Chunk           string                  `json:"chunk,omitempty"`
	Failure         string                  `json:"failure,omitempty"`
	SkillVersion    string                  `json:"skill_version,omitempty"`
	SkillSource     string                  `json:"skill_source,omitempty"`
	Executor        string                  `json:"executor,omitempty"`

	// Todo checklist.
	Todos []tools.Todo `json:"todos,omitempty"`

	// Model / thinking.
	Text         string `json:"text,omitempty"`
	PromptTokens int    `json:"prompt_tokens,omitempty"`
	ElapsedMS    int64  `json:"elapsed_ms,omitempty"`

	// Compaction.
	BeforeTokens int     `json:"before_tokens,omitempty"`
	AfterTokens  int     `json:"after_tokens,omitempty"`
	SavedTokens  int     `json:"saved_tokens,omitempty"`
	SummaryChars int     `json:"summary_chars,omitempty"`
	Ratio        float64 `json:"ratio,omitempty"`

	// Background jobs (P8.7). job_finished's process exit code; omitted when 0
	// (text "exited" already means success) — present when failed (>0, or -1 for
	// a start failure / signal kill).
	ExitCode int `json:"exit_code,omitempty"`

	Err string `json:"err,omitempty"`
}

// rfc3339Millis is the timestamp format on the wire: RFC3339 with millisecond
// precision, always UTC. Cross-language clients parse it natively.
const rfc3339Millis = "2006-01-02T15:04:05.000Z07:00"

// toWire is the pure Layer-2 mapping from a core event to its wire form. It does
// NOT assign event_id or parent_session_id — the emitter owns those (transport
// identity), which keeps this function deterministic for golden tests.
func toWire(e agent.Event) wireEvent {
	w := wireEvent{
		Kind:            string(e.Kind),
		At:              e.At.UTC().Format(rfc3339Millis),
		SessionID:       e.SessionID,
		TurnID:          e.TurnID,
		InvocationID:    e.InvocationID,
		Seq:             e.Seq,
		CallID:          e.CallID,
		Step:            e.Step,
		ToolName:        e.ToolName,
		ToolArgs:        toWireArgs(e.ToolArgs),
		Observation:     e.Observation,
		Output:          e.Output,
		Assets:          e.Assets,
		TextAnnotations: e.TextAnnotations,
		Chunk:           e.Chunk,
		Failure:         e.Failure,
		SkillVersion:    e.Version,
		SkillSource:     e.SkillSource,
		Executor:        e.Executor,
		Todos:           e.Todos,
		Text:            e.Text,
		PromptTokens:    e.PromptTokens,
		BeforeTokens:    e.BeforeTokens,
		AfterTokens:     e.AfterTokens,
		SavedTokens:     e.SavedTokens,
		SummaryChars:    e.SummaryChars,
		Ratio:           e.Ratio,
		ExitCode:        e.ExitCode,
		Err:             e.Err,
	}
	// Duration goes out as milliseconds, never Go's default nanoseconds (§3.2).
	if e.Elapsed > 0 {
		w.ElapsedMS = e.Elapsed.Milliseconds()
	}
	return w
}

// toWireArgs turns the core's JSON-string tool args into structured JSON on the
// wire (§3.2). Empty -> omitted. Valid JSON -> embedded object as-is. Anything
// else -> encoded as a JSON string so the frame stays valid JSON.
func toWireArgs(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}
