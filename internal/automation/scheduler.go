package automation

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Scheduler is the daemon-resident automation loop. It is orthogonal to
// conversation.TurnScheduler (admission concurrency) and internal/jobs
// (background commands): this loop owns "when to fire". It polls the Store for
// due automations and dispatches each one through a Dispatcher.
type Scheduler struct {
	store      Store
	dispatcher Dispatcher
	interval   time.Duration

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	done    chan struct{}
}

// NewScheduler creates a scheduler. interval is the poll cadence; <=0 defaults to
// 30s. dispatcher may be nil (then due automations are enumerated but not fired —
// useful for the skipped-on-reconcile path and tests).
func NewScheduler(store Store, dispatcher Dispatcher, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Scheduler{
		store:      store,
		dispatcher: dispatcher,
		interval:   interval,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// Start launches the loop in a background goroutine. Calling Start twice is a
// no-op for the second call. On first start it reconciles overdue-but-unattended
// automations (R15: skip, don't re-fire).
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	s.running = true
	go s.loop()
}

// Reconcile marks overdue automations as skipped (R15). It returns the number
// skipped. Callers invoke it once at daemon startup, before Start, so a daemon
// that was down while an automation came due does not fire a backlog.
func (s *Scheduler) Reconcile(ctx context.Context) (int, error) {
	return s.store.SkipDue(ctx, time.Now())
}

// Stop terminates the loop. It is safe to call even after Stop already ran.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stop)
	s.mu.Unlock()
	<-s.done
}

// Done returns a channel closed when the loop stops. It lets daemon shutdown wait
// for a clean stop.
func (s *Scheduler) Done() <-chan struct{} { return s.done }

func (s *Scheduler) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	// Run once immediately so overdue tasks are picked up at startup.
	s.tick(context.Background())
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.tick(context.Background())
		}
	}
}

// tick enumerates due automations and dispatches each. Firing failures are
// recorded (failed run + last_error) and never break the loop.
func (s *Scheduler) tick(ctx context.Context) {
	due, err := s.store.NextDueAt(ctx, time.Now())
	if err != nil {
		return
	}
	for _, a := range due {
		s.fire(ctx, a)
	}
}

// fire dispatches one automation and records the outcome. It guards against the
// same automation being fired concurrently by two ticks (the runtime_state
// running flag) and advances next_run_at only after a successful dispatch.
func (s *Scheduler) fire(ctx context.Context, a Automation) {
	if s.dispatcher == nil {
		return
	}
	// Prevent double-fire: mark running in runtime_state first.
	_ = s.store.UpdateRuntimeState(ctx, RuntimeState{
		AutomationID:     a.ID,
		Running:          true,
		RunningStartedAt: time.Now().UTC(),
	})

	turnID, err := s.dispatcher.Dispatch(ctx, a)
	now := time.Now().UTC()

	// Always persist a run record and clear running state.
	if err != nil {
		// A user-cancelled firing (the user stopped the conversation mid-run) is
		// NOT a failure: do not retry it. once → COMPLETED (no next firing);
		// recurring → advance to the next scheduled firing (it will run again at
		// its normal cadence, but not via the 1-minute retry).
		if errors.Is(err, context.Canceled) {
			_ = s.store.RecordRun(ctx, Run{
				AutomationID: a.ID,
				Status:       RunCanceled,
				CreatedAt:    now,
			})
			_ = s.store.UpdateRuntimeState(ctx, RuntimeState{
				AutomationID: a.ID,
				Running:      false,
				LastRunAt:    now,
				LastError:    "cancelled by user",
			})
			var next time.Time
			status := a.Status
			if a.ScheduleType == ScheduleRecurring {
				if computed, cerr := computeNextRunAt(a, now); cerr == nil {
					next = computed
				} else {
					next = now.Add(s.interval)
				}
			} else {
				status = StatusCompleted
			}
			_, _ = s.store.Update(ctx, a.ID, AutomationPatch{
				LastStatus: strPtr(RunCanceled),
				Status:     &status,
				LastRunAt:  timePtr(now),
			})
			_ = s.store.UpdateNextRunAt(ctx, a.ID, next)
			return
		}

		_ = s.store.RecordRun(ctx, Run{
			AutomationID: a.ID,
			Status:       RunFailed,
			CreatedAt:    now,
		})
		_ = s.store.UpdateRuntimeState(ctx, RuntimeState{
			AutomationID: a.ID,
			Running:      false,
			LastRunAt:    now,
			LastError:    err.Error(),
		})
		// Retry policy: a failed firing retries up to MaxRetries times (1 minute
		// apart), then the automation is marked COMPLETED so it stops. The user can
		// re-enable it from the client to try again. This prevents an infinite
		// retry loop that would otherwise create a backlog of failed runs (and
		// orphan conversations) when e.g. the model is out of quota.
		retries := a.RetryCount + 1
		var next time.Time
		status := a.Status
		if retries < MaxRetries {
			next = now.Add(RetryInterval)
		} else {
			status = StatusCompleted
		}
		_, _ = s.store.Update(ctx, a.ID, AutomationPatch{
			LastStatus: strPtr(RunFailed),
			Status:     &status,
			RetryCount: &retries,
			LastRunAt:  timePtr(now),
		})
		_ = s.store.UpdateNextRunAt(ctx, a.ID, next)
		return
	}

	// A workflow-mode skip (overlap policy) returns ("", nil): record it as
	// skipped, not running, and advance to the next firing.
	if a.WorkflowRef != "" && turnID == "" {
		_ = s.store.RecordRun(ctx, Run{
			AutomationID: a.ID,
			Status:       RunSkipped,
			CreatedAt:    now,
		})
		_ = s.store.UpdateRuntimeState(ctx, RuntimeState{
			AutomationID: a.ID,
			Running:      false,
			LastRunAt:    now,
			LastError:    "skipped: active workflow run",
		})
		var next time.Time
		status := a.Status
		if a.ScheduleType == ScheduleRecurring {
			if computed, cerr := computeNextRunAt(a, now); cerr == nil {
				next = computed
			} else {
				next = now.Add(s.interval)
			}
		} else {
			status = StatusCompleted
		}
		_, _ = s.store.Update(ctx, a.ID, AutomationPatch{
			LastRunAt:  timePtr(now),
			LastStatus: strPtr(RunSkipped),
			Status:     &status,
			RetryCount: intPtr(0),
		})
		_ = s.store.UpdateNextRunAt(ctx, a.ID, next)
		return
	}

	_ = s.store.RecordRun(ctx, Run{
		AutomationID: a.ID,
		SessionID:    turnID,
		TaskID:       taskIDField(turnID),
		Status:       RunRunning,
		CreatedAt:    now,
	})
	_ = s.store.UpdateRuntimeState(ctx, RuntimeState{
		AutomationID:  a.ID,
		Running:       false,
		LastRunAt:     now,
		RunningTurnID: turnID,
	})

	// Reuse mode: persist the firing's conversation id so later firings return to
	// the same conversation. This also covers the case where the persisted
	// conversation was deleted and the dispatcher created a fresh one (the new id
	// differs from the stored one and replaces it). Workflow mode returns a task
	// id, never a session id — never persist it as the reuse session.
	if a.WorkflowRef == "" && a.ModeExec == ModeReuse && turnID != "" && turnID != a.SessionID {
		_, _ = s.store.Update(ctx, a.ID, AutomationPatch{SessionID: &turnID})
	}

	// Advance next_run_at for recurring automations; for once automations the
	// firing is terminal: next_run_at is zeroed (stops rescheduling) and the
	// status becomes COMPLETED so the control panel shows "finished" instead of
	// ACTIVE (WorkBuddy: "一次性任务（执行一次后自动结束）").
	var next time.Time
	status := a.Status
	if a.ScheduleType == ScheduleRecurring {
		computed, err := computeNextRunAt(a, now)
		if err == nil {
			next = computed
		} else {
			next = now.Add(s.interval)
		}
	} else {
		status = StatusCompleted
	}
	_, _ = s.store.Update(ctx, a.ID, AutomationPatch{
		LastRunAt:  timePtr(now),
		LastStatus: strPtr(RunRunning),
		Status:     &status,
		// A successful firing resets the consecutive-failure counter.
		RetryCount: intPtr(0),
	})
	_ = s.store.UpdateNextRunAt(ctx, a.ID, next)
}

func strPtr(s string) *string        { return &s }
func timePtr(t time.Time) *time.Time { return &t }
func intPtr(i int) *int              { return &i }
