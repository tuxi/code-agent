package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/runtime"
)

func TestTaskProgressShowsStepsAndErases(t *testing.T) {
	var buf bytes.Buffer
	p := newTaskProgress(&buf)

	p.Emit(agent.Event{Kind: agent.EventTaskStarted})
	p.Emit(agent.Event{Kind: agent.EventModelStarted}) // iteration 1 — the budgeted unit
	p.Emit(agent.Event{Kind: agent.EventToolStarted, Step: 3, ToolName: "read_file"})
	p.Emit(agent.Event{Kind: agent.EventTaskFinished})

	out := buf.String()
	// "step" tracks iterations vs the budget — NOT the tool-call ordinal (Step:3).
	want := fmt.Sprintf("step 1/%d", runtime.SubAgentMaxSteps)
	if !strings.Contains(out, "subagent") || !strings.Contains(out, want) || !strings.Contains(out, "read_file") {
		t.Fatalf("heartbeat should show %q and the current tool, got: %q", want, out)
	}
	if strings.Contains(out, "step 3/") {
		t.Fatalf("heartbeat must not use the tool-call ordinal as the step count, got: %q", out)
	}
	// Every update overwrites in place (\r) and the final state erases the line.
	if !strings.Contains(out, "\r\x1b[K") {
		t.Fatalf("heartbeat should rewrite/clear its line in place, got: %q", out)
	}
	if !strings.HasSuffix(out, "\r\x1b[K") {
		t.Fatalf("a finished task should erase its heartbeat line, got: %q", out)
	}
}

func TestTaskProgressIgnoresUnrelatedEvents(t *testing.T) {
	var buf bytes.Buffer
	p := newTaskProgress(&buf)
	// Only the task bookends, model-call starts (iterations), and tool starts
	// drive the heartbeat; everything else is noise for it.
	p.Emit(agent.Event{Kind: agent.EventThinking, Text: "hmm"})
	p.Emit(agent.Event{Kind: agent.EventToolFinished})
	p.Emit(agent.Event{Kind: agent.EventObserved})
	if buf.Len() != 0 {
		t.Fatalf("events unrelated to the heartbeat should not render, got: %q", buf.String())
	}
}

func TestMultiEmitterFansOutAndSkipsNil(t *testing.T) {
	a, b := &recordingEmitter{}, &recordingEmitter{}
	m := runtime.MultiEmitter{a, nil, b} // nil must be skipped, not panic
	m.Emit(agent.Event{Kind: agent.EventToolStarted})
	if len(a.got) != 1 || len(b.got) != 1 {
		t.Fatalf("both real sinks should receive the event: a=%v b=%v", a.got, b.got)
	}
}

func TestSubAgentRunDrivesProgress(t *testing.T) {
	rec := &recordingEmitter{}
	sa := testSubAgent(answerProvider{content: "done"}, t.TempDir())
	sa.Progress = rec
	if _, err := sa.Run(context.Background(), "", "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The progress sink must see the bookends even with no store wired.
	var sawStart, sawFinish bool
	for _, e := range rec.got {
		sawStart = sawStart || e.Kind == agent.EventTaskStarted
		sawFinish = sawFinish || e.Kind == agent.EventTaskFinished
	}
	if !sawStart || !sawFinish {
		t.Fatalf("progress should be bracketed by task_started/finished, got %v", rec.got)
	}
}
