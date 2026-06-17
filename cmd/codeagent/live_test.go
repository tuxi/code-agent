package main

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
)

type recordingEmitter struct{ got []agent.Event }

func (r *recordingEmitter) Emit(e agent.Event) { r.got = append(r.got, e) }

// safeBuffer serializes the ticker goroutine's writes with the test's reads.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// liveProgress is a transparent decorator: every event reaches next, and the
// ticker starts/stops on the model events without dropping or mangling anything.
func TestLiveProgressForwardsAllEvents(t *testing.T) {
	rec := &recordingEmitter{}
	lp := newLiveProgress(rec, io.Discard)

	events := []agent.Event{
		{Kind: agent.EventTurnStarted},
		{Kind: agent.EventModelStarted},
		{Kind: agent.EventModelFinished},
		{Kind: agent.EventToolStarted, ToolName: "read_file"},
		{Kind: agent.EventToolFinished, Observation: "x"},
		{Kind: agent.EventTurnFinished, Text: "done"},
	}
	for _, e := range events {
		lp.Emit(e)
	}

	if len(rec.got) != len(events) {
		t.Fatalf("forwarded %d events, want %d (decorator must be transparent)", len(rec.got), len(events))
	}
	if rec.got[len(rec.got)-1].Text != "done" {
		t.Fatalf("last forwarded event lost its data: %+v", rec.got[len(rec.got)-1])
	}
}

// A ModelStarted whose ModelFinished never arrives (e.g. the model call errored)
// must still be cleanly stoppable: stopAndClear is idempotent and never
// double-closes the stop channel.
func TestLiveProgressStopIsIdempotent(t *testing.T) {
	lp := newLiveProgress(&recordingEmitter{}, io.Discard)
	lp.Emit(agent.Event{Kind: agent.EventModelStarted})
	lp.stopAndClear()
	lp.stopAndClear() // must be a no-op, not a panic
}

// While the model call is in flight, the ticker rewrites a "Thinking… Ns" line.
func TestLiveProgressTicksWhileWaiting(t *testing.T) {
	var buf safeBuffer
	lp := newLiveProgress(&recordingEmitter{}, &buf)
	lp.Emit(agent.Event{Kind: agent.EventModelStarted})
	time.Sleep(1100 * time.Millisecond) // long enough for one tick
	lp.Emit(agent.Event{Kind: agent.EventModelFinished})
	if !strings.Contains(buf.String(), "Thinking") {
		t.Fatalf("expected a 'Thinking...' tick while waiting, got %q", buf.String())
	}
}
