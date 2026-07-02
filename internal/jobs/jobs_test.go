package jobs

import (
	"strings"
	"sync"
	"testing"
)

func TestStartAndComplete(t *testing.T) {
	r := NewRegistry()
	job := r.Start(".", "echo hi", []string{"echo", "hi"}, Owner{})
	job.Wait()

	s := job.Snapshot()
	if s.Status != Exited {
		t.Errorf("status = %q, want exited", s.Status)
	}
	if s.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", s.ExitCode)
	}
	if !strings.Contains(job.Logs(), "hi") {
		t.Errorf("logs = %q, want them to contain hi", job.Logs())
	}
	if _, ok := r.Get(s.ID); !ok {
		t.Error("job not found in registry")
	}
}

func TestFailingJob(t *testing.T) {
	r := NewRegistry()
	job := r.Start(".", "false", []string{"false"}, Owner{})
	job.Wait()

	s := job.Snapshot()
	if s.Status != Failed {
		t.Errorf("status = %q, want failed", s.Status)
	}
	if s.ExitCode == 0 {
		t.Error("exit_code = 0, want nonzero")
	}
}

func TestCancel(t *testing.T) {
	r := NewRegistry()
	job := r.Start(".", "sleep 30", []string{"sleep", "30"}, Owner{})

	if st := job.Snapshot().Status; st != Running {
		t.Fatalf("status = %q, want running", st)
	}
	if err := r.Cancel(job.Snapshot().ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	job.Wait()
	if st := job.Snapshot().Status; st != Canceled {
		t.Errorf("status = %q, want canceled", st)
	}
	// Cancelling a finished job is an error, not a panic.
	if err := r.Cancel(job.Snapshot().ID); err == nil {
		t.Error("expected an error cancelling an already-finished job")
	}
}

func TestStartNonexistentBinary(t *testing.T) {
	r := NewRegistry()
	job := r.Start(".", "no-such-binary-xyz", []string{"no-such-binary-xyz"}, Owner{})
	job.Wait()
	if st := job.Snapshot().Status; st != Failed {
		t.Errorf("status = %q, want failed", st)
	}
}

func TestListInStartOrder(t *testing.T) {
	r := NewRegistry()
	r.Start(".", "echo a", []string{"echo", "a"}, Owner{}).Wait()
	r.Start(".", "echo b", []string{"echo", "b"}, Owner{}).Wait()

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	if list[0].Command != "echo a" || list[1].Command != "echo b" {
		t.Errorf("List order = [%q, %q], want [echo a, echo b]", list[0].Command, list[1].Command)
	}
}

func TestCancelUnknownJob(t *testing.T) {
	r := NewRegistry()
	if err := r.Cancel("job_999"); err == nil {
		t.Error("expected an error cancelling an unknown job")
	}
}

// recordingSink captures Sink callbacks for assertions. Mutex-guarded because
// callbacks arrive from job goroutines (the Sink contract).
type recordingSink struct {
	mu       sync.Mutex
	started  []string // commands
	output   []byte
	finished []Snapshot
}

func (s *recordingSink) JobStarted(snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, snap.Command)
}
func (s *recordingSink) JobOutput(id string, chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output = append(s.output, chunk...)
}
func (s *recordingSink) JobFinished(snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, snap)
}

func (s *recordingSink) state() (started []string, output string, finished []Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.started...), string(s.output), append([]Snapshot(nil), s.finished...)
}

func TestSinkObservesLifecycle(t *testing.T) {
	sink := &recordingSink{}
	r := NewRegistry()
	r.Sink = sink

	job := r.Start(".", "echo tee-marker", []string{"echo", "tee-marker"}, Owner{})
	job.Wait()

	started, output, finished := sink.state()
	if len(started) != 1 || started[0] != "echo tee-marker" {
		t.Errorf("started = %v, want the command", started)
	}
	if !strings.Contains(output, "tee-marker") {
		t.Errorf("sink output = %q, want it to contain tee-marker", output)
	}
	if len(finished) != 1 || finished[0].Status != Exited {
		t.Errorf("finished = %+v, want one exited snapshot", finished)
	}
	// The tee must not starve the buffer: job_logs still sees the output.
	if !strings.Contains(job.Logs(), "tee-marker") {
		t.Errorf("Logs() = %q, want tee-marker despite the sink tee", job.Logs())
	}
}

func TestSinkStartFailureStillPairs(t *testing.T) {
	sink := &recordingSink{}
	r := NewRegistry()
	r.Sink = sink

	r.Start(".", "no-such-binary-xyz", []string{"no-such-binary-xyz"}, Owner{}).Wait()

	started, _, finished := sink.state()
	if len(started) != 1 {
		t.Errorf("started count = %d, want 1", len(started))
	}
	if len(finished) != 1 || finished[0].Status != Failed {
		t.Errorf("finished = %+v, want exactly one failed snapshot (Started/Finished must pair)", finished)
	}
}

func TestSinkObservesCancel(t *testing.T) {
	sink := &recordingSink{}
	r := NewRegistry()
	r.Sink = sink

	job := r.Start(".", "sleep 30", []string{"sleep", "30"}, Owner{})
	if err := r.Cancel(job.Snapshot().ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	job.Wait()

	_, _, finished := sink.state()
	if len(finished) != 1 || finished[0].Status != Canceled {
		t.Errorf("finished = %+v, want one canceled snapshot", finished)
	}
}
