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

// TestSchedulerCancelDoesNotRetry verifies a user-cancelled firing is NOT
// retried: once → COMPLETED, recurring → advanced to the next scheduled firing,
// and retry_count is not incremented (a cancel is not a failure).
func TestSchedulerCancelDoesNotRetry(t *testing.T) {
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
	_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))

	disp := &failDispatcher{err: context.Canceled}
	sched := NewScheduler(s, disp, time.Hour)
	sched.tick(context.Background())

	got, _ := s.Get(context.Background(), created.ID)
	if got.RetryCount != 0 {
		t.Fatalf("retry_count = %d after cancel, want 0 (cancel is not a failure)", got.RetryCount)
	}
	if got.LastStatus != RunCanceled {
		t.Fatalf("last_status = %q, want canceled", got.LastStatus)
	}
	// Recurring: next_run_at advanced to the next scheduled firing (not a 1-min
	// retry), so it is not due again immediately.
	if got.NextRunAt.IsZero() {
		t.Fatal("next_run_at should be advanced to the next scheduled firing")
	}
	due, _ := s.NextDueAt(context.Background(), time.Now().Add(time.Second))
	if len(due) != 0 {
		t.Fatalf("cancelled recurring automation should not be due again this tick, got %d", len(due))
	}
}

// TestSchedulerCancelOnceCompletes verifies a cancelled once firing becomes
// COMPLETED and never fires again.
func TestSchedulerCancelOnceCompletes(t *testing.T) {
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
	disp := &failDispatcher{err: context.Canceled}
	sched := NewScheduler(s, disp, time.Hour)
	sched.tick(context.Background())

	got, _ := s.Get(context.Background(), created.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("cancelled once automation status = %q, want COMPLETED", got.Status)
	}
	due, _ := s.NextDueAt(context.Background(), time.Now().Add(time.Hour))
	if len(due) != 0 {
		t.Fatalf("completed once automation should not be due, got %d", len(due))
	}
}

// TestSchedulerReusePersistsSession verifies a reuse-mode recurring automation
// creates a conversation on the first firing, persists it as session_id, and
// later firings reuse it (no conversation pile-up).
func TestSchedulerReusePersistsSession(t *testing.T) {
	s := newTestStore(t)
	a := Automation{
		Name:         "btc-poll",
		Prompt:       "query btc",
		Status:       StatusActive,
		ScheduleType: ScheduleRecurring,
		RRule:        "FREQ=MINUTELY;INTERVAL=5",
		Timezone:     "UTC",
		ModeExec:     ModeReuse,
	}
	created, _ := s.Create(context.Background(), a)
	disp := &fakeDispatcher{}
	sched := NewScheduler(s, disp, time.Hour)

	// First firing: creates a conversation, scheduler persists session_id.
	_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))
	sched.tick(context.Background())
	got, _ := s.Get(context.Background(), created.ID)
	if got.SessionID == "" {
		t.Fatal("reuse mode should persist the first firing's session_id")
	}

	// Second firing: dispatcher receives the persisted session_id.
	_ = s.UpdateNextRunAt(context.Background(), created.ID, time.Now().Add(-time.Second))
	sched.tick(context.Background())
	if len(disp.fired) != 2 {
		t.Fatalf("expected 2 firings, got %d", len(disp.fired))
	}
	// The dispatcher was given the persisted session on the second firing.
	if disp.fired[1].SessionID != got.SessionID {
		t.Fatalf("second firing session_id = %q, want %q (reused)", disp.fired[1].SessionID, got.SessionID)
	}
}

// TestDispatcherReuseFirstFiring verifies the dispatcher creates a conversation
// on the first reuse firing and reuses the persisted id on later ones.
func TestDispatcherReuseFirstFiring(t *testing.T) {
	creator := &trackingCreator{}
	sub := &recordingSubmitter{}
	d := NewTurnDispatcher(sub, creator)

	// First firing: no session_id yet → create.
	a := Automation{ID: "auto-1", Prompt: "p", ModeExec: ModeReuse}
	sid, err := d.Dispatch(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "sess-1" {
		t.Fatalf("first firing sid = %q, want sess-1", sid)
	}

	// Second firing: session_id persisted → reuse, no new conversation.
	a2 := Automation{ID: "auto-1", Prompt: "p", ModeExec: ModeReuse, SessionID: "sess-1"}
	sid2, err := d.Dispatch(context.Background(), a2)
	if err != nil {
		t.Fatal(err)
	}
	if sid2 != "sess-1" {
		t.Fatalf("second firing sid = %q, want sess-1 (reused)", sid2)
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
