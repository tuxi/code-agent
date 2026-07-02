package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/jobs"
	"code-agent/internal/session/sqlite"
)

// recordingEmitter captures events; mutex-guarded because the sink emits from
// job goroutines and flush timers.
type recordingEmitter struct {
	mu     sync.Mutex
	events []agent.Event
}

func (r *recordingEmitter) Emit(e agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingEmitter) snapshot() []agent.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Event(nil), r.events...)
}

func TestJobEventSinkLifecycle(t *testing.T) {
	rec := &recordingEmitter{}
	sink := NewJobEventSink(rec)

	sink.JobStarted("job_1", "npm install")
	sink.JobOutput("job_1", []byte("chunk-a "))
	sink.JobOutput("job_1", []byte("chunk-b"))
	sink.JobFinished("job_1", jobs.Snapshot{ID: "job_1", Status: jobs.Exited, DurationMS: 1500})

	evs := rec.snapshot()
	if len(evs) != 3 {
		t.Fatalf("want 3 events (started, coalesced output, finished), got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != agent.EventJobStarted || evs[0].SessionID != "job_1" || evs[0].Text != "npm install" {
		t.Errorf("started = %+v", evs[0])
	}
	// The two small writes must coalesce into ONE output event (flushed by
	// JobFinished, well before the timer).
	if evs[1].Kind != agent.EventJobOutput || evs[1].Chunk != "chunk-a chunk-b" {
		t.Errorf("output = %+v, want one coalesced chunk", evs[1])
	}
	if evs[2].Kind != agent.EventJobFinished || evs[2].Text != "exited" || evs[2].Elapsed != 1500*time.Millisecond {
		t.Errorf("finished = %+v", evs[2])
	}
}

func TestJobEventSinkFailureCarriesExitCode(t *testing.T) {
	rec := &recordingEmitter{}
	sink := NewJobEventSink(rec)

	sink.JobFinished("job_2", jobs.Snapshot{ID: "job_2", Status: jobs.Failed, ExitCode: 2})

	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].Err != "exit code 2" {
		t.Errorf("Err = %q, want exit code 2", evs[0].Err)
	}
	if evs[0].ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2 (structured field for client display logic)", evs[0].ExitCode)
	}
}

func TestJobEventSinkSizeFlush(t *testing.T) {
	rec := &recordingEmitter{}
	sink := NewJobEventSink(rec)

	// One write over the size threshold flushes immediately, no timer wait.
	big := strings.Repeat("x", jobFlushBytes)
	sink.JobOutput("job_3", []byte(big))

	evs := rec.snapshot()
	if len(evs) != 1 || evs[0].Kind != agent.EventJobOutput {
		t.Fatalf("want an immediate size-triggered output flush, got %+v", evs)
	}
	if len(evs[0].Chunk) != jobFlushBytes {
		t.Errorf("chunk len = %d, want %d", len(evs[0].Chunk), jobFlushBytes)
	}
}

func TestJobEventSinkTimerFlush(t *testing.T) {
	rec := &recordingEmitter{}
	sink := NewJobEventSink(rec)

	sink.JobOutput("job_4", []byte("slow trickle"))
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("small chunk should buffer, not emit; got %d events", n)
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(rec.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	evs := rec.snapshot()
	if len(evs) != 1 || evs[0].Chunk != "slow trickle" {
		t.Fatalf("want one timer-flushed output event, got %+v", evs)
	}
}

func TestJobEventSinkInterleavedJobs(t *testing.T) {
	rec := &recordingEmitter{}
	sink := NewJobEventSink(rec)

	sink.JobOutput("job_a", []byte("from-a"))
	sink.JobOutput("job_b", []byte("from-b"))
	sink.JobFinished("job_a", jobs.Snapshot{ID: "job_a", Status: jobs.Exited})

	// job_a's finish must flush ONLY job_a's buffer.
	for _, ev := range rec.snapshot() {
		if ev.SessionID == "job_b" {
			t.Errorf("job_b buffer flushed by job_a's finish: %+v", ev)
		}
		if ev.Kind == agent.EventJobOutput && ev.SessionID == "job_a" && ev.Chunk != "from-a" {
			t.Errorf("job_a chunk = %q", ev.Chunk)
		}
	}

	sink.JobFinished("job_b", jobs.Snapshot{ID: "job_b", Status: jobs.Exited})
	var bChunk string
	for _, ev := range rec.snapshot() {
		if ev.Kind == agent.EventJobOutput && ev.SessionID == "job_b" {
			bChunk = ev.Chunk
		}
	}
	if bChunk != "from-b" {
		t.Errorf("job_b chunk = %q, want from-b", bChunk)
	}
}

// TestJobEventsReplayableByJobID is the end-to-end proof of the P8.7 Phase A
// "Done when" criterion: a real background job, observed through the real
// sqlite store, leaves a replayable event stream under the JOB's own id — the
// exact partition GET /v1/conversations/{job_id}/events reads.
func TestJobEventsReplayableByJobID(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer store.Close()

	reg := jobs.NewRegistry()
	reg.Sink = NewJobEventSink(EventStoreEmitter{Ctx: context.Background(), Store: store})

	job := reg.Start(".", "echo replay-marker", []string{"echo", "replay-marker"})
	job.Wait()
	jobID := job.Snapshot().ID

	recs, err := store.SessionEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("SessionEvents(%s): %v", jobID, err)
	}
	var kinds []string
	var sawMarker bool
	for _, rec := range recs {
		kinds = append(kinds, rec.Kind)
		if strings.Contains(string(rec.Payload), "replay-marker") {
			sawMarker = true
		}
	}
	want := []string{"job_started", "job_output", "job_finished"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if !sawMarker {
		t.Error("job output chunk not found in the persisted payloads")
	}
}
