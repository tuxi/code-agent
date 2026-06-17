package jobs

import (
	"strings"
	"testing"
)

func TestStartAndComplete(t *testing.T) {
	r := NewRegistry()
	job := r.Start(".", "echo hi", []string{"echo", "hi"})
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
	job := r.Start(".", "false", []string{"false"})
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
	job := r.Start(".", "sleep 30", []string{"sleep", "30"})

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
	job := r.Start(".", "no-such-binary-xyz", []string{"no-such-binary-xyz"})
	job.Wait()
	if st := job.Snapshot().Status; st != Failed {
		t.Errorf("status = %q, want failed", st)
	}
}

func TestListInStartOrder(t *testing.T) {
	r := NewRegistry()
	r.Start(".", "echo a", []string{"echo", "a"}).Wait()
	r.Start(".", "echo b", []string{"echo", "b"}).Wait()

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
