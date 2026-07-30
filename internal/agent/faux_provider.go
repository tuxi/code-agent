package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"code-agent/internal/model"
)

// FauxStep describes one response the fake LLM should return. Exactly one of
// Text, ToolCalls, or Error should be set; others are zero-valued.
//
// A FauxStep is the Go equivalent of Pi's FauxResponseStep:
//
//	FauxText("ok")        → text response
//	FauxTool("read", ...)  → tool call(s)
//	FauxThink("hmm...")   → thinking + empty text
//	FauxError("timeout")   → simulated error
type FauxStep struct {
	// Text is the assistant's plain-text response. When set, FinishReason
	// defaults to "stop".
	Text string

	// ToolCalls lists tools the model wants the runtime to execute. When set,
	// FinishReason defaults to "tool_calls".
	ToolCalls []FauxToolCall

	// Thinking is provider-visible reasoning. It appears in ReasoningContent
	// on the response but does not change the FinishReason.
	Thinking string

	// Delay simulates network latency before the response is returned.
	Delay time.Duration

	// Error, when non-empty, makes Complete return this string as an error.
	// This is the mechanism for testing error-recovery paths in the loop.
	Error string
}

// FauxToolCall is a simplified tool-call declaration for test code.
// The ID is auto-generated; Args is serialised to the JSON string the
// wire format expects.
type FauxToolCall struct {
	Name string
	Args map[string]any
}

// FauxProvider is a fake model.Provider that returns pre-programmed steps
// in order. Each call to Complete dequeues the next step; when the queue is
// exhausted it returns an error so tests don't silently pass with empty
// responses.
//
// FauxProvider implements model.Provider. It does NOT implement
// model.StreamingProvider — streaming tests should use a real provider or
// a separate streaming fake.
//
// This is the Go equivalent of Pi's registerFauxProvider() / FauxProviderRegistration.
type FauxProvider struct {
	mu    sync.Mutex
	Steps []FauxStep

	// CallCount tracks how many times Complete has been invoked.
	CallCount int

	// LastRequest is the most recent model.Request received by Complete.
	// Tests assert on it to verify what context reached the model.
	LastRequest model.Request
}

// Compile-time interface check.
var _ model.Provider = (*FauxProvider)(nil)

// Complete returns the next pre-programmed FauxStep. Each call advances the
// internal cursor. When no steps remain it returns an error.
func (p *FauxProvider) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.LastRequest = req
	idx := p.CallCount
	p.CallCount++

	if idx >= len(p.Steps) {
		return model.Response{}, fmt.Errorf("faux: no response programmed for call %d (have %d steps)", idx, len(p.Steps))
	}

	step := p.Steps[idx]

	// Simulate latency. Respects context cancellation so tests can set deadlines.
	if step.Delay > 0 {
		select {
		case <-time.After(step.Delay):
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
	}

	// Simulate error.
	if step.Error != "" {
		return model.Response{}, fmt.Errorf("faux: %s", step.Error)
	}

	// Assemble response.
	resp := model.Response{
		Content:          step.Text,
		ReasoningContent: step.Thinking,
	}

	if len(step.ToolCalls) > 0 {
		resp.ToolCalls = make([]model.ToolCall, len(step.ToolCalls))
		for i, tc := range step.ToolCalls {
			argsJSON, err := json.Marshal(tc.Args)
			if err != nil {
				return model.Response{}, fmt.Errorf("faux: marshal tool call %q args: %w", tc.Name, err)
			}
			resp.ToolCalls[i] = model.ToolCall{
				ID:   fmt.Sprintf("faux_tc_%d_%d", idx, i),
				Type: "function",
				Function: model.FunctionCall{
					Name:      tc.Name,
					Arguments: string(argsJSON),
				},
			}
		}
		resp.FinishReason = "tool_calls"
	} else if step.Text != "" || step.Thinking != "" {
		resp.FinishReason = "stop"
	}

	// Strip whitespace-only content (the loop treats this as a no-op).
	if strings.TrimSpace(resp.Content) == "" && !resp.HasToolCalls() {
		resp.Content = ""
	}

	return resp, nil
}

// SetResponses replaces the step queue. Safe to call between turns.
func (p *FauxProvider) SetResponses(steps []FauxStep) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Steps = steps
	p.CallCount = 0
}

// AppendResponses adds steps to the end of the queue without resetting the
// cursor. Use this to add follow-up steps mid-test when the agent will make
// additional model calls.
func (p *FauxProvider) AppendResponses(steps []FauxStep) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Steps = append(p.Steps, steps...)
}

// PendingCalls returns how many calls remain before the provider is exhausted.
func (p *FauxProvider) PendingCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Steps) - p.CallCount
}

// ── Constructor helpers ────────────────────────────────────────────────

// FauxText returns a step that produces a plain-text response.
func FauxText(text string) FauxStep {
	return FauxStep{Text: text}
}

// FauxTool returns a step that produces a single tool call.
func FauxTool(name string, args map[string]any) FauxStep {
	return FauxStep{ToolCalls: []FauxToolCall{{Name: name, Args: args}}}
}

// FauxTools returns a step that produces multiple tool calls in one response.
func FauxTools(calls ...FauxToolCall) FauxStep {
	return FauxStep{ToolCalls: calls}
}

// FauxThink returns a step with thinking content and an optional text response.
func FauxThink(thinking string, text ...string) FauxStep {
	s := FauxStep{Thinking: thinking}
	if len(text) > 0 {
		s.Text = text[0]
	}
	return s
}

// FauxError returns a step that makes Complete return an error.
func FauxError(msg string) FauxStep {
	return FauxStep{Error: msg}
}
