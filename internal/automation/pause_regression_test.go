package automation

import (
	"context"
	"testing"
	"time"
)

// Regression tests for the two automation lifecycle bugs reported 2026-09-15/16
// (x-skills/codeagent-bug-automation-pause-overwrite.md):
//
//	Bug #1: scheduler.fire() held the PRE-TURN automation snapshot and wrote
//	        `status := a.Status` back at turn end, silently undoing any pause
//	        performed while the turn was running (self-pause or an external
//	        `update enabled=false`) — the write-back is the +2ms UpdatedAt
//	        resurrection seen in the report's controlled experiment.
//	Bug #2: a recurring automation that hit the retry cap was marked COMPLETED
//	        (terminal) with NextRunAt zeroed to epoch, and `enabled=true` did
//	        not re-arm the schedule or reset RetryCount — so recovery was
//	        impossible without delete+recreate.

// midTurnPauseDispatcher simulates a pause landing WHILE the turn runs: Submit
// blocks for the whole turn, so the pause is applied to the store after fire()
// captured its snapshot and before fire() performs its write-back.
type midTurnPauseDispatcher struct {
	s     Store
	ret   string
	err   error
	calls int
}

func (m *midTurnPauseDispatcher) Dispatch(ctx context.Context, a Automation) (string, error) {
	m.calls++
	paused := StatusPaused
	if _, err := m.s.Update(ctx, a.ID, AutomationPatch{Status: &paused}); err != nil {
		return "", err
	}
	return m.ret, m.err
}

// TestSchedulerWriteBackPreservesMidTurnPause is the automated form of the
// report's event-4 controlled experiment, across all four write-back paths
// (success, failure, cancel, workflow-skip). The turn-end bookkeeping must
// update run fields but must NEVER resurrect a paused automation.
func TestSchedulerWriteBackPreservesMidTurnPause(t *testing.T) {
	cases := []struct {
		name        string
		ret         string
		err         error
		workflowRef string
	}{
		{"success", "sess-1", nil, ""},
		{"failed", "", context.DeadlineExceeded, ""},
		{"canceled", "", context.Canceled, ""},
		{"workflow-skip", "", nil, "/ws#template"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			created, err := s.Create(context.Background(), Automation{
				Name:         "poll",
				Prompt:       "query",
				Status:       StatusActive,
				ScheduleType: ScheduleRecurring,
				RRule:        "FREQ=MINUTELY;INTERVAL=45",
				Timezone:     "UTC",
				ModeExec:     ModeStandalone,
				WorkflowRef:  tc.workflowRef,
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))

			disp := &midTurnPauseDispatcher{s: s, ret: tc.ret, err: tc.err}
			sched := NewScheduler(s, disp, time.Hour)
			sched.tick(context.Background())
			if disp.calls != 1 {
				t.Fatalf("expected 1 firing, got %d", disp.calls)
			}

			got, _ := s.Get(context.Background(), created.ID)
			if got.Status != StatusPaused {
				t.Fatalf("turn-end write-back resurrected the automation: status = %q, want PAUSED", got.Status)
			}
			// Bookkeeping still lands: the firing is recorded and the schedule
			// advanced (a paused row is filtered by status, so advancing is safe).
			if got.LastRunAt.IsZero() {
				t.Fatal("LastRunAt should be updated by the write-back")
			}
			if got.NextRunAt.IsZero() {
				t.Fatal("NextRunAt should be advanced (retry or cadence), not left at epoch")
			}
		})
	}
}

// TestSchedulerSuccessRecordsSucceeded verifies the success write-back records
// the firing as succeeded (Submit blocks until the turn ends, so "running" was
// a state that never updated — the report's minor observation).
func TestSchedulerSuccessRecordsSucceeded(t *testing.T) {
	s := newTestStore(t)
	created, _ := s.Create(context.Background(), Automation{
		Name:         "poll",
		Prompt:       "query",
		Status:       StatusActive,
		ScheduleType: ScheduleRecurring,
		RRule:        "FREQ=MINUTELY;INTERVAL=5",
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	})
	_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))

	sched := NewScheduler(s, &fakeDispatcher{}, time.Hour)
	sched.tick(context.Background())

	got, _ := s.Get(context.Background(), created.ID)
	if got.LastStatus != RunSucceeded {
		t.Fatalf("last_status = %q, want %q", got.LastStatus, RunSucceeded)
	}
	if got.Status != StatusActive {
		t.Fatalf("status = %q, want ACTIVE (unchanged by a successful recurring firing)", got.Status)
	}
	runs, _ := s.ListRuns(context.Background(), created.ID, 10)
	if len(runs) != 1 || runs[0].Status != RunSucceeded {
		t.Fatalf("run records = %+v, want one %q", runs, RunSucceeded)
	}
}

// TestReEnableReArmsStaleSchedule is the Bug #2 revival half: after the retry
// cap left the row COMPLETED/PAUSED with NextRunAt at epoch and RetryCount at
// the cap, `enabled=true` (a non-ACTIVE → ACTIVE patch) must reset the failure
// counter and recompute NextRunAt from now — not leave a stale epoch row that
// either fires immediately off-grid or is re-killed by its first failure.
func TestReEnableReArmsStaleSchedule(t *testing.T) {
	s := newTestStore(t)
	created, _ := s.Create(context.Background(), Automation{
		Name:         "poll",
		Prompt:       "query",
		Status:       StatusActive,
		ScheduleType: ScheduleRecurring,
		RRule:        "FREQ=MINUTELY;INTERVAL=45",
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	})

	// Simulate the retry-cap end state the scheduler writes today.
	completed := StatusCompleted
	rc := MaxRetries
	_, _ = s.Update(context.Background(), created.ID, AutomationPatch{Status: &completed, RetryCount: &rc})
	_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Time{})

	// Revive.
	active := StatusActive
	updated, err := s.Update(context.Background(), created.ID, AutomationPatch{Status: &active})
	if err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if updated.RetryCount != 0 {
		t.Fatalf("retry_count = %d after re-enable, want 0", updated.RetryCount)
	}
	if !updated.NextRunAt.After(time.Now()) {
		t.Fatalf("next_run_at = %v after re-enable, want a future on-cadence time (not stale epoch)", updated.NextRunAt)
	}
	// It must NOT be due immediately (the stale-epoch off-grid firing).
	due, _ := s.NextDueAt(context.Background(), time.Now())
	if len(due) != 0 {
		t.Fatalf("re-enabled automation should not fire immediately, got %d due", len(due))
	}
}

// TestRecurringCapPauseIsRevivable ties the two halves together: the cap now
// pauses (not completes) a recurring task, and re-enabling it makes it due
// again on cadence — the delete+recreate workaround must no longer be needed.
func TestRecurringCapPauseIsRevivable(t *testing.T) {
	s := newTestStore(t)
	created, _ := s.Create(context.Background(), Automation{
		Name:         "poll",
		Prompt:       "query",
		Status:       StatusActive,
		ScheduleType: ScheduleRecurring,
		RRule:        "FREQ=MINUTELY;INTERVAL=45",
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	})
	disp := &failDispatcher{err: context.DeadlineExceeded}
	sched := NewScheduler(s, disp, time.Hour)
	for i := 0; i < MaxRetries; i++ {
		_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))
		sched.tick(context.Background())
	}

	active := StatusActive
	updated, err := s.Update(context.Background(), created.ID, AutomationPatch{Status: &active})
	if err != nil {
		t.Fatalf("re-enable after cap: %v", err)
	}
	if updated.Status != StatusActive || updated.RetryCount != 0 {
		t.Fatalf("revival state: status=%q retry=%d, want ACTIVE/0", updated.Status, updated.RetryCount)
	}
	// Make the next firing due and let a healthy dispatcher run it: the task
	// must recover fully (counter stays 0, cadence resumes, still ACTIVE).
	_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))
	ok := &fakeDispatcher{}
	NewScheduler(s, ok, time.Hour).tick(context.Background())
	got, _ := s.Get(context.Background(), created.ID)
	if got.Status != StatusActive {
		t.Fatalf("after recovery status = %q, want ACTIVE", got.Status)
	}
	if got.RetryCount != 0 || got.NextRunAt.IsZero() || !got.NextRunAt.After(time.Now()) {
		t.Fatalf("after recovery: retry=%d next=%v, want 0/future", got.RetryCount, got.NextRunAt)
	}
}
