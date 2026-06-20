package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"code-agent/internal/agent"
)

func TestTaskProgressShowsStepsAndErases(t *testing.T) {
	var buf bytes.Buffer
	p := newTaskProgress(&buf)

	p.Emit(agent.Event{Kind: agent.EventTaskStarted})
	p.Emit(agent.Event{Kind: agent.EventToolStarted, Step: 3, ToolName: "read_file"})
	p.Emit(agent.Event{Kind: agent.EventTaskFinished})

	out := buf.String()
	if !strings.Contains(out, "subagent") || !strings.Contains(out, "step 3") || !strings.Contains(out, "read_file") {
		t.Fatalf("heartbeat should show the current step/tool, got: %q", out)
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
	// Model/thinking events of the subagent are noise for a heartbeat — only
	// tool starts and the task bookends drive it.
	p.Emit(agent.Event{Kind: agent.EventModelStarted})
	p.Emit(agent.Event{Kind: agent.EventThinking, Text: "hmm"})
	if buf.Len() != 0 {
		t.Fatalf("non-tool events should not render, got: %q", buf.String())
	}
}

func TestMultiEmitterFansOutAndSkipsNil(t *testing.T) {
	a, b := &recordingEmitter{}, &recordingEmitter{}
	m := multiEmitter{a, nil, b} // nil must be skipped, not panic
	m.Emit(agent.Event{Kind: agent.EventToolStarted})
	if len(a.got) != 1 || len(b.got) != 1 {
		t.Fatalf("both real sinks should receive the event: a=%v b=%v", a.got, b.got)
	}
}

func TestSubAgentRunDrivesProgress(t *testing.T) {
	rec := &recordingEmitter{}
	sa := testSubAgent(answerProvider{content: "done"}, t.TempDir())
	sa.progress = rec
	if _, err := sa.Run(context.Background(), "go"); err != nil {
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
