package session

import (
	"testing"
	"time"
)

func TestLifecycleHelpers(t *testing.T) {
	s := &Session{} // nil Metadata: helpers must be nil-safe

	if s.TurnStatus() != "" {
		t.Errorf("fresh status=%q want empty", s.TurnStatus())
	}
	if s.PausedAt() != 0 {
		t.Errorf("fresh paused_at=%d want 0", s.PausedAt())
	}

	now := time.Unix(1_700_000_000, 0)
	s.MarkPaused(now)
	if s.TurnStatus() != TurnStatusPaused {
		t.Errorf("status=%q want paused", s.TurnStatus())
	}
	if s.PausedAt() != now.Unix() {
		t.Errorf("paused_at=%d want %d", s.PausedAt(), now.Unix())
	}

	if n := s.IncResumeAttempts(); n != 1 {
		t.Errorf("first inc=%d want 1", n)
	}
	if n := s.IncResumeAttempts(); n != 2 {
		t.Errorf("second inc=%d want 2", n)
	}
	if s.ResumeAttempts() != 2 {
		t.Errorf("attempts=%d want 2", s.ResumeAttempts())
	}
	s.ClearResumeAttempts()
	if s.ResumeAttempts() != 0 {
		t.Errorf("after clear attempts=%d want 0", s.ResumeAttempts())
	}
}

func TestExecutionPolicyDefaultsToShared(t *testing.T) {
	s := &Session{}
	if got := s.ExecutionPolicy(); got != ExecutionPolicySharedWorkspace {
		t.Fatalf("default policy = %q", got)
	}
	s.SetExecutionPolicy(ExecutionPolicyIsolatedWorktree)
	if got := s.ExecutionPolicy(); got != ExecutionPolicyIsolatedWorktree {
		t.Fatalf("policy = %q", got)
	}
	s.SetExecutionPolicy("unknown")
	if got := s.ExecutionPolicy(); got != ExecutionPolicySharedWorkspace {
		t.Fatalf("unknown policy = %q, want shared", got)
	}
}
