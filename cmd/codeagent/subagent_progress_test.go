package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/runtime"
	"code-agent/internal/tools"
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

// TestTaskProgressConcurrentSubagents drives the heartbeat from many goroutines
// at once — the parallel-`task` case (P8.8). It must not race (run with -race)
// and must collapse to an aggregate line while >1 subagent is active, then erase
// once all finish.
func TestTaskProgressConcurrentSubagents(t *testing.T) {
	p := newTaskProgress(&syncBuf{})
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("sub_%d", i)
			p.Emit(agent.Event{Kind: agent.EventTaskStarted, SessionID: sid})
			p.Emit(agent.Event{Kind: agent.EventModelStarted, SessionID: sid})
			p.Emit(agent.Event{Kind: agent.EventToolStarted, SessionID: sid, ToolName: "grep"})
			p.Emit(agent.Event{Kind: agent.EventTaskFinished, SessionID: sid})
		}(i)
	}
	wg.Wait()

	// All finished → no subagents tracked, and the line is erased.
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.subs) != 0 {
		t.Errorf("subs = %d, want 0 after all finished", len(p.subs))
	}
	if p.shown {
		t.Error("heartbeat line should be erased once every subagent finished")
	}
}

// TestTaskProgressAggregateLine: with several subagents active, the heartbeat
// shows the collapsed "N subagents running" form rather than one subagent's line.
func TestTaskProgressAggregateLine(t *testing.T) {
	var buf bytes.Buffer
	p := newTaskProgress(&buf)
	p.Emit(agent.Event{Kind: agent.EventTaskStarted, SessionID: "a"})
	p.Emit(agent.Event{Kind: agent.EventModelStarted, SessionID: "a"})
	p.Emit(agent.Event{Kind: agent.EventTaskStarted, SessionID: "b"})
	p.Emit(agent.Event{Kind: agent.EventModelStarted, SessionID: "b"})

	out := buf.String()
	if !strings.Contains(out, "2 subagents running") {
		t.Fatalf("want the aggregate line for 2 active subagents, got: %q", out)
	}
}

// syncBuf is a mutex-guarded io.Writer so the test's own writes don't race
// (the heartbeat serializes its writes under p.mu, but the test asserts state,
// not output, here).
type syncBuf struct {
	mu sync.Mutex
}

func (s *syncBuf) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(b), nil
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
	if _, err := sa.Run(context.Background(), tools.ExecutionContext{}, "go"); err != nil {
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
