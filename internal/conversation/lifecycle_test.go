package conversation

import (
	"context"
	"errors"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

// scriptRunner returns a preset outcome and records which method ran.
type scriptRunner struct {
	resumeErr error
	ranResume bool
}

func (r *scriptRunner) RunTurn(ctx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	return agent.TurnResult{}, nil
}
func (r *scriptRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	r.ranResume = true
	return agent.TurnResult{}, r.resumeErr
}

type scriptBuilder struct{ runner TurnRunner }

func (b *scriptBuilder) Build(ctx RuntimeContext) TurnRunner { return b.runner }

func newExecutorWith(repo *fakeRepo, runner TurnRunner) *TurnExecutor {
	return NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &scriptBuilder{runner: runner})
}

func pausedSession(id string) *session.Session {
	s := &session.Session{
		ID: id,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "hi"},
		},
		Metadata: map[string]any{},
	}
	s.MarkPaused(time.Now())
	return s
}

func TestResume_SuccessMarksDoneAndClearsAttempts(t *testing.T) {
	repo := newFakeRepo()
	s := pausedSession("s1")
	s.Metadata[session.MetaResumeAttempts] = float64(3)
	repo.sessions["s1"] = s
	runner := &scriptRunner{resumeErr: nil}
	ex := newExecutorWith(repo, runner)

	if _, err := ex.Resume(context.Background(), "s1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !runner.ranResume {
		t.Fatal("ResumeTurn was not invoked")
	}
	got := repo.sessions["s1"]
	if got.TurnStatus() != session.TurnStatusDone {
		t.Errorf("status=%q want done", got.TurnStatus())
	}
	if got.ResumeAttempts() != 0 {
		t.Errorf("attempts=%d want 0 (cleared on success)", got.ResumeAttempts())
	}
}

func TestResume_RetryableRepausesAndIncrements(t *testing.T) {
	repo := newFakeRepo()
	repo.sessions["s1"] = pausedSession("s1")
	ex := newExecutorWith(repo, &scriptRunner{resumeErr: &model.APIError{StatusCode: 503}})

	_, _ = ex.Resume(context.Background(), "s1")
	got := repo.sessions["s1"]
	if got.TurnStatus() != session.TurnStatusPaused {
		t.Errorf("status=%q want paused (retryable)", got.TurnStatus())
	}
	if got.ResumeAttempts() != 1 {
		t.Errorf("attempts=%d want 1", got.ResumeAttempts())
	}
}

func TestResume_NonRetryableFails(t *testing.T) {
	repo := newFakeRepo()
	repo.sessions["s1"] = pausedSession("s1")
	ex := newExecutorWith(repo, &scriptRunner{resumeErr: errors.New("bad request")})

	_, _ = ex.Resume(context.Background(), "s1")
	if got := repo.sessions["s1"].TurnStatus(); got != session.TurnStatusFailed {
		t.Errorf("status=%q want failed (non-retryable)", got)
	}
}

func TestResume_ExceedsRetryCapFails(t *testing.T) {
	repo := newFakeRepo()
	s := pausedSession("s1")
	s.Metadata[session.MetaResumeAttempts] = float64(maxResumeAttempts) // already at the cap
	repo.sessions["s1"] = s
	ex := newExecutorWith(repo, &scriptRunner{resumeErr: &model.APIError{StatusCode: 503}})

	_, _ = ex.Resume(context.Background(), "s1")
	if got := repo.sessions["s1"].TurnStatus(); got != session.TurnStatusFailed {
		t.Errorf("status=%q want failed (over retry cap)", got)
	}
}

// blockingRunner parks until the turn context is cancelled, so a test can suspend
// it mid-flight.
type blockingRunner struct{ started chan struct{} }

func (r *blockingRunner) RunTurn(ctx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	close(r.started)
	<-ctx.Done()
	return agent.TurnResult{}, ctx.Err()
}
func (r *blockingRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	return agent.TurnResult{}, nil
}

func TestSuspendAll_MarksInFlightTurnPaused(t *testing.T) {
	repo := newFakeRepo()
	sess := &session.Session{
		ID: "s1",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "hi"},
		},
		Metadata: map[string]any{},
	}
	repo.sessions["s1"] = sess
	br := &blockingRunner{started: make(chan struct{})}
	ex := newExecutorWith(repo, br)

	done := make(chan struct{})
	go func() {
		_, _ = ex.ExecuteWithSession(context.Background(), sess, "hi")
		close(done)
	}()
	<-br.started // the turn is in flight, blocked on ctx

	ids := ex.SuspendAll(context.Background())
	<-done // the turn has unwound and saved

	if len(ids) != 1 || ids[0] != "s1" {
		t.Fatalf("SuspendAll returned %v, want [s1]", ids)
	}
	got := repo.sessions["s1"]
	if got.TurnStatus() != session.TurnStatusPaused {
		t.Errorf("status=%q want paused", got.TurnStatus())
	}
	if got.PausedAt() == 0 {
		t.Error("paused_at was not recorded")
	}
}

// TestReconcileInterrupted rewrites a running/resuming status to paused, and
// leaves done/normal sessions untouched.
func TestReconcileInterrupted(t *testing.T) {
	repo := newFakeRepo()
	running := &session.Session{ID: "run", Messages: []model.Message{{Role: model.RoleUser, Content: "x"}}, Metadata: map[string]any{}}
	running.SetTurnStatus(session.TurnStatusRunning)
	done := &session.Session{ID: "done", Messages: []model.Message{{Role: model.RoleUser, Content: "y"}}, Metadata: map[string]any{}}
	done.SetTurnStatus(session.TurnStatusDone)
	repo.sessions["run"] = running
	repo.sessions["done"] = done
	// fakeRepo.List returns nil, so drive reconciliation off an explicit meta list.
	repo.metas = []session.Meta{
		{ID: "run", TurnStatus: session.TurnStatusRunning},
		{ID: "done", TurnStatus: session.TurnStatusDone},
	}
	ex := newExecutorWith(repo, &scriptRunner{})

	if err := ex.ReconcileInterrupted(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := repo.sessions["run"].TurnStatus(); got != session.TurnStatusPaused {
		t.Errorf("running session status=%q want paused", got)
	}
	if got := repo.sessions["done"].TurnStatus(); got != session.TurnStatusDone {
		t.Errorf("done session status=%q want unchanged done", got)
	}
}
