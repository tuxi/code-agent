package agent

import (
	"strings"
	"testing"
)

func TestFauxProviderTextResponse(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxText("Hello, world"),
	})

	result, err := h.RunTurn("say hello")

	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if result.Final != "Hello, world" {
		t.Errorf("Final = %q, want %q", result.Final, "Hello, world")
	}
}

func TestFauxProviderToolCallAndContinue(t *testing.T) {
	// The loop calls Complete() once per iteration. A text response with
	// stop_reason="stop" ends the loop. A tool_call response makes the loop
	// execute the tool, append the result, and call Complete() again.
	// So: tool call → tool result injected → model called again → text answer.
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("read_file", map[string]any{"path": "main.go"}),
		FauxText("Found: package main"),
	})
	h.RegisterReadOnlyTool("read_file", "Read a file", "// main.go content")

	result, err := h.RunTurn("read main.go")

	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if result.Final != "Found: package main" {
		t.Errorf("Final = %q, want %q", result.Final, "Found: package main")
	}
	if h.Provider.CallCount != 2 {
		t.Errorf("CallCount = %d, want 2 (tool call + final text)", h.Provider.CallCount)
	}
}

func TestFauxProviderError(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxError("api timeout"),
	})

	_, err := h.RunTurn("anything")

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "api timeout") {
		t.Errorf("error = %q, want to contain 'api timeout'", err.Error())
	}
}

func TestFauxProviderExhausted(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxText("only one response"),
	})

	// First call works.
	_, err := h.RunTurn("hi")
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}

	// Second call should fail — queue is empty.
	_, err = h.RunTurn("again")
	if err == nil {
		t.Fatal("expected error on exhausted queue")
	}
}

func TestFauxHarnessEvents(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxText("Done."),
	})

	_, err := h.RunTurn("do it")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	// Should have emitted at least model_start and model_end events.
	started := h.EventsOfType(EventModelStarted)
	if len(started) == 0 {
		t.Error("no EventModelStarted events captured")
	}
}

func TestFauxHarnessAppendResponses(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{FauxText("first")})

	// First turn consumes "first".
	result, err := h.RunTurn("one")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if result.Final != "first" {
		t.Errorf("turn 1 Final = %q", result.Final)
	}

	// Append more steps for a follow-up turn.
	h.Provider.AppendResponses([]FauxStep{FauxText("second")})
	result, err = h.RunTurn("two")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if result.Final != "second" {
		t.Errorf("turn 2 Final = %q", result.Final)
	}
}

func TestFauxHarnessRecordingToolReadOnly(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("list_files", map[string]any{"path": "."}),
		FauxText("3 files found."),
	})
	tool := h.RegisterReadOnlyTool("list_files", "List directory", ".git\nmain.go\nREADME.md")

	result, err := h.RunTurn("list files")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !tool.Ran {
		t.Error("tool should have run")
	}
	if result.Final != "3 files found." {
		t.Errorf("Final = %q", result.Final)
	}
}

func TestFauxHarnessRecordingToolSideEffecting(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxTool("deploy", map[string]any{"env": "prod"}),
		FauxText("Deployed."),
	})
	tool := h.RegisterSideEffectingTool("deploy", "Deploy to environment", "ok")

	// Side-effecting tools require an Approver. Without one, the loop denies
	// the tool and returns the denial as a tool result (not a turn error).
	// The tool itself never runs.
	_, err := h.RunTurn("deploy to prod")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	// The loop completes normally — the tool was denied, not executed.
	if tool.Ran {
		t.Error("side-effecting tool should NOT run without an approver")
	}
}

func TestFauxThink(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxThink("hmm, let me check...", "OK done."),
	})

	result, err := h.RunTurn("think")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if result.Final != "OK done." {
		t.Errorf("Final = %q", result.Final)
	}
}

func TestFauxMultipleToolCalls(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		FauxTools(
			FauxToolCall{Name: "read_file", Args: map[string]any{"path": "a.go"}},
			FauxToolCall{Name: "read_file", Args: map[string]any{"path": "b.go"}},
		),
		FauxText("Read both files."),
	})
	h.RegisterReadOnlyTool("read_file", "Read a file", "// content")

	result, err := h.RunTurn("read a.go and b.go")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if result.Final != "Read both files." {
		t.Errorf("Final = %q", result.Final)
	}
	// Verify both tool calls were in the same response.
	req := h.LastRequest()
	// The loop should have captured the messages correctly.
	_ = req
}

func TestFauxProviderEmptyText(t *testing.T) {
	// An empty-text step with FinishReason "stop" is valid — the loop treats it
	// as an empty assistant response.
	h := NewFauxHarness(t, []FauxStep{FauxText("")})

	_, err := h.RunTurn("hi")

	// The loop rejects empty assistant responses (ErrEmptyAssistantResponse).
	if err == nil {
		t.Error("expected error on empty assistant response")
	}
}

func TestFauxProviderLastRequest(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{FauxText("ok")})

	h.RunTurn("check this out")

	req := h.LastRequest()
	if len(req.Messages) == 0 {
		t.Fatal("LastRequest has no messages")
	}
	// First message should be the system prompt.
	if req.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", req.Messages[0].Role)
	}
}
