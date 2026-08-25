package automation

import (
	"context"
	"testing"
	"time"
)

// failDispatcher always fails the submit, so the standalone conversation is
// created but orphaned.
type failDispatcher struct {
	err error
}

func (f *failDispatcher) Dispatch(ctx context.Context, a Automation) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "", context.DeadlineExceeded
}

// TestSchedulerFailureAdvancesNextRun verifies a recurring automation whose
// firing fails is retried (next_run_at advanced by RetryInterval), not retried
// every tick (Problem 3).
func TestSchedulerFailureAdvancesNextRun(t *testing.T) {
	s := newTestStore(t)
	a := Automation{
		Name:         "poll",
		Prompt:       "query",
		Status:       StatusActive,
		ScheduleType: ScheduleRecurring,
		RRule:        "FREQ=MINUTELY;INTERVAL=5",
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	}
	created, err := s.Create(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	// Force next_run_at to the past so it is immediately due.
	_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Minute))

	disp := &failDispatcher{err: context.DeadlineExceeded}
	sched := NewScheduler(s, disp, time.Hour)
	sched.tick(context.Background())

	got, _ := s.Get(context.Background(), created.ID)
	if got.NextRunAt.IsZero() {
		t.Fatal("next_run_at should be advanced after a failed firing, not zero")
	}
	if got.NextRunAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("next_run_at still in the past: %v (would retry every tick)", got.NextRunAt)
	}
	if got.LastStatus != RunFailed {
		t.Fatalf("last_status = %q, want failed", got.LastStatus)
	}
	if got.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1 after first failure", got.RetryCount)
	}
	// It must not be due again now (retry is 1 minute out).
	due, _ := s.NextDueAt(context.Background(), time.Now().Add(time.Second))
	if len(due) != 0 {
		t.Fatalf("failed recurring automation should not be due again this tick, got %d", len(due))
	}
}

// TestSchedulerFailureCompletesAfterMaxRetries verifies that after MaxRetries
// consecutive failures the automation is marked COMPLETED and stops retrying.
func TestSchedulerFailureCompletesAfterMaxRetries(t *testing.T) {
	s := newTestStore(t)
	a := Automation{
		Name:         "poll",
		Prompt:       "query",
		Status:       StatusActive,
		ScheduleType: ScheduleRecurring,
		RRule:        "FREQ=MINUTELY;INTERVAL=5",
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	}
	created, _ := s.Create(context.Background(), a)
	disp := &failDispatcher{err: context.DeadlineExceeded}
	sched := NewScheduler(s, disp, time.Hour)

	// Fire MaxRetries times (each tick advances next_run_at by RetryInterval, so
	// force it due before each tick).
	for i := 0; i < MaxRetries; i++ {
		_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))
		sched.tick(context.Background())
	}

	got, _ := s.Get(context.Background(), created.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("after %d failures status = %q, want COMPLETED", MaxRetries, got.Status)
	}
	if got.RetryCount != MaxRetries {
		t.Fatalf("retry_count = %d, want %d", got.RetryCount, MaxRetries)
	}
	// It must not be due again.
	due, _ := s.NextDueAt(context.Background(), time.Now().Add(time.Hour))
	if len(due) != 0 {
		t.Fatalf("completed automation should not be due, got %d", len(due))
	}
}

// TestSchedulerSuccessResetsRetryCount verifies a successful firing after
// failures resets the counter.
func TestSchedulerSuccessResetsRetryCount(t *testing.T) {
	s := newTestStore(t)
	a := Automation{
		Name:         "poll",
		Prompt:       "query",
		Status:       StatusActive,
		ScheduleType: ScheduleRecurring,
		RRule:        "FREQ=MINUTELY;INTERVAL=5",
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	}
	created, _ := s.Create(context.Background(), a)
	// Simulate 2 prior failures.
	rc := 2
	_, _ = s.Update(context.Background(), created.ID, AutomationPatch{RetryCount: &rc})

	disp := &fakeDispatcher{}
	sched := NewScheduler(s, disp, time.Hour)
	_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))
	sched.tick(context.Background())

	got, _ := s.Get(context.Background(), created.ID)
	if got.RetryCount != 0 {
		t.Fatalf("retry_count = %d after success, want 0", got.RetryCount)
	}
}

// TestSchedulerFailureOnceCompletes verifies a once automation that fails
// retries up to MaxRetries then becomes COMPLETED (won't re-run forever).
func TestSchedulerFailureOnceCompletes(t *testing.T) {
	s := newTestStore(t)
	a := Automation{
		Name:         "once",
		Prompt:       "do it",
		Status:       StatusActive,
		ScheduleType: ScheduleOnce,
		ScheduledAt:  time.Now().Add(-time.Minute),
		Timezone:     "UTC",
		ModeExec:     ModeStandalone,
	}
	created, _ := s.Create(context.Background(), a)
	disp := &failDispatcher{err: context.DeadlineExceeded}
	sched := NewScheduler(s, disp, time.Hour)
	for i := 0; i < MaxRetries; i++ {
		_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))
		sched.tick(context.Background())
	}

	got, _ := s.Get(context.Background(), created.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("failed once automation status = %q, want COMPLETED after %d retries", got.Status, MaxRetries)
	}
	due, _ := s.NextDueAt(context.Background(), time.Now().Add(time.Hour))
	if len(due) != 0 {
		t.Fatalf("completed once automation should not be due, got %d", len(due))
	}
}

// TestDispatcherKeepsConversationOnFailure verifies a standalone firing whose
// submit fails KEEPS the just-created conversation (so the user can open it and
// see the failure), rather than rolling it back. The retry cap bounds how many
// such conversations a failing automation can create.
func TestDispatcherKeepsConversationOnFailure(t *testing.T) {
	deleted := ""
	creator := &trackingCreator{onDelete: func(id string) { deleted = id }}
	sub := &failingSubmitter{}
	d := NewTurnDispatcher(sub, creator)

	a := Automation{
		ID:       "auto-1",
		Prompt:   "p",
		ModeExec: ModeStandalone,
	}
	if _, err := d.Dispatch(context.Background(), a); err == nil {
		t.Fatal("expected dispatch error")
	}
	if deleted != "" {
		t.Fatalf("conversation should be kept on failure, but was deleted: %q", deleted)
	}
}

type trackingCreator struct {
	onDelete func(id string)
}

func (t *trackingCreator) CreateConversation(ctx context.Context, workspacePath string) (string, error) {
	return "sess-1", nil
}

func (t *trackingCreator) DeleteConversation(ctx context.Context, sessionID string) error {
	if t.onDelete != nil {
		t.onDelete(sessionID)
	}
	return nil
}

type failingSubmitter struct{}

func (f *failingSubmitter) Submit(ctx context.Context, sessionID, prompt, model string, perm Perm) (string, error) {
	return "", context.DeadlineExceeded
}
