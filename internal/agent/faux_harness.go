package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

// FauxHarness is a one-call test setup that wires a FauxProvider, an in-memory
// session, a tool registry, a Runner, and event capture into one value.
//
// Usage:
//
//	h := NewFauxHarness(t, []FauxStep{
//	    FauxText("Let me look at the file."),
//	    FauxTool("read_file", map[string]any{"path": "main.go"}),
//	    FauxText("Found 3 TODOs."),
//	})
//	result, err := h.RunTurn("find TODOs")
//	// assert result, events, messages...
//
// This is the Go equivalent of Pi's createHarness() in test/suite/harness.ts.
type FauxHarness struct {
	T *testing.T

	Provider *FauxProvider
	Session  *session.Session
	Runner   *Runner
	Tools    *tools.Registry
	Events   []Event
}

// NewFauxHarness builds a harness with default settings. Steps are the
// pre-programmed LLM responses. Tools are registered but empty by default;
// add real tool implementations via h.Tools.Register(...).
func NewFauxHarness(t *testing.T, steps []FauxStep) *FauxHarness {
	t.Helper()

	reg := tools.NewRegistry()
	provider := &FauxProvider{Steps: steps}
	workspace := t.TempDir()
	sess, err := session.NewBuilder(workspace).Build()
	if err != nil {
		t.Fatalf("faux harness: build session: %v", err)
	}

	runner := &Runner{
		Model:         provider,
		Tools:         reg,
		MaxSteps:      10,
		WorkspaceRoot: workspace,
	}

	h := &FauxHarness{
		T:        t,
		Provider: provider,
		Session:  sess,
		Runner:   runner,
		Tools:    reg,
	}

	// Wire event capture.
	runner.Emitter = &fauxEmitter{h: h}
	return h
}

// RunTurn is a convenience wrapper for h.Runner.RunTurn. It returns the
// turn result and collects all emitted events into h.Events.
func (h *FauxHarness) RunTurn(userInput string) (TurnResult, error) {
	h.Events = nil
	return h.Runner.RunTurn(context.Background(), h.Session, userInput)
}

// LastRequest returns the most recent model.Request seen by the provider.
func (h *FauxHarness) LastRequest() model.Request {
	return h.Provider.LastRequest
}

// PendingCalls returns how many faux steps remain.
func (h *FauxHarness) PendingCalls() int {
	return h.Provider.PendingCalls()
}

// UserMessages returns the user-role messages in the session (excludes system).
func (h *FauxHarness) UserMessages() []model.Message {
	var out []model.Message
	for _, m := range h.Session.Messages {
		if m.Role == model.RoleUser {
			out = append(out, m)
		}
	}
	return out
}

// AssistantMessages returns the assistant-role messages in the session.
func (h *FauxHarness) AssistantMessages() []model.Message {
	var out []model.Message
	for _, m := range h.Session.Messages {
		if m.Role == model.RoleAssistant {
			out = append(out, m)
		}
	}
	return out
}

// EventsOfType filters captured events by type.
func (h *FauxHarness) EventsOfType(kind EventKind) []Event {
	var out []Event
	for _, e := range h.Events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// fauxEmitter collects events into the harness.
type fauxEmitter struct {
	h *FauxHarness
}

func (e *fauxEmitter) Emit(ev Event) {
	e.h.Events = append(e.h.Events, ev)
}

// ── Tool helpers for tests ──────────────────────────────────────────────

// RegisterReadOnlyTool registers a simple read-only tool that records its
// invocation. Read-only tools skip the approver — the agent loop runs them
// automatically.
func (h *FauxHarness) RegisterReadOnlyTool(name, description string, result string) *RecordingTool {
	t := &RecordingTool{
		NameVal:        name,
		DescriptionVal: description,
		Result:         result,
	}
	h.Tools.Register(t)
	return t
}

// RegisterSideEffectingTool is like RegisterReadOnlyTool but implements
// tools.SideEffecting so the agent loop routes it through the Approver.
func (h *FauxHarness) RegisterSideEffectingTool(name, description string, result string) *RecordingTool {
	t := &RecordingTool{
		NameVal:        name,
		DescriptionVal: description,
		Result:         result,
		sideEffect:     true,
	}
	h.Tools.Register(t)
	return t
}

// RecordingTool is a test double implementing tools.Tool. It records every
// Execute call (input + whether it ran) and returns a fixed result.
type RecordingTool struct {
	NameVal        string
	DescriptionVal string
	Result         string
	sideEffect     bool

	Ran   bool
	Input json.RawMessage
}

func (t *RecordingTool) Name() string        { return t.NameVal }
func (t *RecordingTool) Description() string { return t.DescriptionVal }
func (t *RecordingTool) InputSchema() json.RawMessage {
	return tools.Object(nil).JSON()
}
func (t *RecordingTool) Execute(_ context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	t.Ran = true
	t.Input = input
	return tools.ToolResult{Content: t.Result}, nil
}

// SideEffects implements tools.SideEffecting. When false (the default for
// RegisterReadOnlyTool), the tool is treated as read-only.
func (t *RecordingTool) SideEffects() bool { return t.sideEffect }

// Compile-time interface checks.
var (
	_ tools.Tool          = (*RecordingTool)(nil)
	_ tools.SideEffecting = (*RecordingTool)(nil)
)
