package jobs

import (
	"errors"
	"sync/atomic"
	"testing"
)

type executionLeaseSink struct {
	acquired, released, started, finished atomic.Int32
	deny                                  bool
}

func (s *executionLeaseSink) AcquireJobLease(string, Owner) (func(), error) {
	s.acquired.Add(1)
	if s.deny {
		return nil, errors.New("denied by durable workspace claim")
	}
	return func() { s.released.Add(1) }, nil
}
func (s *executionLeaseSink) JobStarted(Snapshot)    { s.started.Add(1) }
func (*executionLeaseSink) JobOutput(string, []byte) {}
func (s *executionLeaseSink) JobFinished(Snapshot)   { s.finished.Add(1) }

func TestBackgroundExecutionLeaseReleasedAfterProcessCleanup(t *testing.T) {
	r := NewRegistry()
	sink := &executionLeaseSink{}
	r.Sink = sink
	job := r.Start(t.TempDir(), "echo local", []string{"sh", "-c", "printf local"}, Owner{})
	job.Wait()
	if job.Snapshot().Status != Exited || sink.acquired.Load() != 1 || sink.released.Load() != 1 || sink.started.Load() != 1 || sink.finished.Load() != 1 {
		t.Fatal(job.Snapshot(), sink.acquired.Load(), sink.released.Load())
	}
}
func TestBackgroundExecutionLeaseRejectsBeforeExecAndReleasesStartFailure(t *testing.T) {
	for _, deny := range []bool{false, true} {
		r := NewRegistry()
		sink := &executionLeaseSink{deny: deny}
		r.Sink = sink
		job := r.Start(t.TempDir(), "invalid executable", []string{"/no-such-b2c-command"}, Owner{})
		job.Wait()
		expected := int32(1)
		if deny {
			expected = 0
		}
		if job.Snapshot().Status != Failed || sink.acquired.Load() != 1 || sink.released.Load() != expected || sink.started.Load() != 1 || sink.finished.Load() != 1 {
			t.Fatal(job.Snapshot(), sink.acquired.Load(), sink.released.Load())
		}
	}
}
