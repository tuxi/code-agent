package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/jobs"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
)

// fakeEventStore records EventRecords in memory, assigning seqs like the sqlite
// rowid does. Mutex-guarded: the sink writes from job goroutines and timers.
type fakeEventStore struct {
	mu   sync.Mutex
	rows []session.EventRecord
}

func (f *fakeEventStore) RecordEvent(_ context.Context, e session.EventRecord) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.Seq = int64(len(f.rows) + 1)
	f.rows = append(f.rows, e)
	return e.Seq, nil
}

func (f *fakeEventStore) SessionEvents(_ context.Context, sessionID string) ([]session.EventRecord, error) {
	return f.since(sessionID, 0), nil
}

func (f *fakeEventStore) SessionEventsSince(_ context.Context, sessionID string, sinceSeq int64) ([]session.EventRecord, error) {
	return f.since(sessionID, sinceSeq), nil
}

func (f *fakeEventStore) RecentEventsByKind(_ context.Context, _ string, _ int) ([]session.EventRecord, error) {
	return nil, nil
}

func (f *fakeEventStore) since(sessionID string, sinceSeq int64) []session.EventRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []session.EventRecord
	for _, r := range f.rows {
		if r.SessionID == sessionID && r.Seq > sinceSeq {
			out = append(out, r)
		}
	}
	return out
}

func kindsOf(recs []session.EventRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Kind
	}
	return out
}

// recordingEmitter captures live-forwarded events for the resolver tests.
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

var testOwner = jobs.Owner{SessionID: "sess_parent", TurnID: "turn_3"}

func TestJobEventSinkDualPartition(t *testing.T) {
	store := &fakeEventStore{}
	live := &recordingEmitter{}
	sink := NewJobEventSink(context.Background(), store)
	sink.SetLiveResolver(func(sessionID string) agent.Emitter {
		if sessionID != "sess_parent" {
			t.Errorf("live resolved for %q, want sess_parent", sessionID)
		}
		return live
	})

	sink.JobStarted(jobs.Snapshot{ID: "job_1", Command: "npm install", Owner: testOwner})
	sink.JobOutput("job_1", []byte("chunk-a "))
	sink.JobOutput("job_1", []byte("chunk-b"))
	sink.JobFinished(jobs.Snapshot{ID: "job_1", Status: jobs.Exited, DurationMS: 1500, Owner: testOwner})

	// Child partition: the full stream (brackets + coalesced output).
	child, _ := store.SessionEvents(context.Background(), "job_1")
	wantChild := []string{"job_started", "job_output", "job_finished"}
	if got := kindsOf(child); strings.Join(got, ",") != strings.Join(wantChild, ",") {
		t.Errorf("child partition kinds = %v, want %v", got, wantChild)
	}

	// Parent partition: brackets ONLY — never the output firehose (§8.4-2).
	parent, _ := store.SessionEvents(context.Background(), "sess_parent")
	wantParent := []string{"job_started", "job_finished"}
	if got := kindsOf(parent); strings.Join(got, ",") != strings.Join(wantParent, ",") {
		t.Errorf("parent partition kinds = %v, want %v", got, wantParent)
	}
	// The parent-row payload still carries the JOB's id (child-stream identity)
	// and the starting turn (entry-card placement).
	var ev agent.Event
	if err := json.Unmarshal(parent[0].Payload, &ev); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if ev.SessionID != "job_1" || ev.TurnID != "turn_3" {
		t.Errorf("parent payload session_id=%q turn_id=%q, want job_1/turn_3", ev.SessionID, ev.TurnID)
	}

	// Live copies: brackets only, stamped with the PARENT-partition seq so the
	// client's per-conversation cursor keeps working.
	evs := live.snapshot()
	if len(evs) != 2 {
		t.Fatalf("live events = %d, want 2 (brackets only)", len(evs))
	}
	for i, want := range wantParent {
		if string(evs[i].Kind) != want {
			t.Errorf("live[%d] = %s, want %s", i, evs[i].Kind, want)
		}
		if evs[i].Seq != parent[i].Seq {
			t.Errorf("live[%d].Seq = %d, want parent-partition seq %d", i, evs[i].Seq, parent[i].Seq)
		}
	}
	if evs[1].Text != "exited" || evs[1].Elapsed != 1500*time.Millisecond {
		t.Errorf("live finished = %+v", evs[1])
	}
}

func TestJobEventSinkUnowned(t *testing.T) {
	store := &fakeEventStore{}
	called := false
	sink := NewJobEventSink(context.Background(), store)
	sink.SetLiveResolver(func(string) agent.Emitter { called = true; return nil })

	sink.JobStarted(jobs.Snapshot{ID: "job_9", Command: "make"})
	sink.JobFinished(jobs.Snapshot{ID: "job_9", Status: jobs.Exited})

	child, _ := store.SessionEvents(context.Background(), "job_9")
	if len(child) != 2 {
		t.Errorf("child rows = %d, want 2", len(child))
	}
	store.mu.Lock()
	total := len(store.rows)
	store.mu.Unlock()
	if total != 2 {
		t.Errorf("total rows = %d, want 2 (no parent copies for an unowned job)", total)
	}
	if called {
		t.Error("live resolver must not fire for an unowned job")
	}
}

func TestJobEventSinkFailureCarriesExitCode(t *testing.T) {
	store := &fakeEventStore{}
	sink := NewJobEventSink(context.Background(), store)

	sink.JobFinished(jobs.Snapshot{ID: "job_2", Status: jobs.Failed, ExitCode: 2})

	recs, _ := store.SessionEvents(context.Background(), "job_2")
	if len(recs) != 1 {
		t.Fatalf("want 1 row, got %d", len(recs))
	}
	var ev agent.Event
	if err := json.Unmarshal(recs[0].Payload, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2 (structured field for client display logic)", ev.ExitCode)
	}
	if ev.Err != "exit code 2" {
		t.Errorf("Err = %q, want exit code 2", ev.Err)
	}
}

func TestJobEventSinkSizeFlush(t *testing.T) {
	store := &fakeEventStore{}
	sink := NewJobEventSink(context.Background(), store)

	// One write over the size threshold flushes immediately, no timer wait.
	sink.JobOutput("job_3", []byte(strings.Repeat("x", jobFlushBytes)))

	recs, _ := store.SessionEvents(context.Background(), "job_3")
	if len(recs) != 1 || recs[0].Kind != "job_output" {
		t.Fatalf("want an immediate size-triggered output flush, got %v", kindsOf(recs))
	}
}

func TestJobEventSinkTimerFlush(t *testing.T) {
	store := &fakeEventStore{}
	sink := NewJobEventSink(context.Background(), store)

	sink.JobOutput("job_4", []byte("slow trickle"))
	if recs, _ := store.SessionEvents(context.Background(), "job_4"); len(recs) != 0 {
		t.Fatalf("small chunk should buffer, not persist; got %d rows", len(recs))
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := store.SessionEvents(context.Background(), "job_4"); len(recs) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	recs, _ := store.SessionEvents(context.Background(), "job_4")
	if len(recs) != 1 {
		t.Fatalf("want one timer-flushed output row, got %d", len(recs))
	}
	var ev agent.Event
	_ = json.Unmarshal(recs[0].Payload, &ev)
	if ev.Chunk != "slow trickle" {
		t.Errorf("chunk = %q", ev.Chunk)
	}
}

func TestJobEventSinkInterleavedJobs(t *testing.T) {
	store := &fakeEventStore{}
	sink := NewJobEventSink(context.Background(), store)

	sink.JobOutput("job_a", []byte("from-a"))
	sink.JobOutput("job_b", []byte("from-b"))
	// job_a's finish must flush ONLY job_a's buffer.
	sink.JobFinished(jobs.Snapshot{ID: "job_a", Status: jobs.Exited})

	if recs, _ := store.SessionEvents(context.Background(), "job_b"); len(recs) != 0 {
		t.Errorf("job_b buffer flushed by job_a's finish: %v", kindsOf(recs))
	}

	sink.JobFinished(jobs.Snapshot{ID: "job_b", Status: jobs.Exited})
	recs, _ := store.SessionEvents(context.Background(), "job_b")
	if got := kindsOf(recs); strings.Join(got, ",") != "job_output,job_finished" {
		t.Errorf("job_b kinds = %v", got)
	}
}

// TestJobEventsReplayableByJobID is the end-to-end proof of the P8.7 "Done
// when" criterion, through the real sqlite store: a real background job leaves
// a replayable stream under the JOB's own id (the child-stream partition
// GET /v1/conversations/{job_id}/events reads) AND bracket rows under the
// owning conversation (entry-card discovery on replay, §8.4-2).
func TestJobEventsReplayableByJobID(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer store.Close()

	reg := jobs.NewRegistry()
	reg.Sink = NewJobEventSink(context.Background(), store)

	job := reg.Start(".", "echo replay-marker", []string{"echo", "replay-marker"}, testOwner)
	job.Wait()
	jobID := job.Snapshot().ID

	recs, err := store.SessionEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("SessionEvents(%s): %v", jobID, err)
	}
	if got := kindsOf(recs); strings.Join(got, ",") != "job_started,job_output,job_finished" {
		t.Fatalf("child kinds = %v", got)
	}
	var sawMarker bool
	for _, rec := range recs {
		if strings.Contains(string(rec.Payload), "replay-marker") {
			sawMarker = true
		}
	}
	if !sawMarker {
		t.Error("job output chunk not found in the persisted payloads")
	}

	parent, err := store.SessionEvents(context.Background(), "sess_parent")
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(parent); strings.Join(got, ",") != "job_started,job_finished" {
		t.Fatalf("parent kinds = %v, want brackets only", got)
	}
}
