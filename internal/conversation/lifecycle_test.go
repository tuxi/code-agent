package conversation

import (
	"context"
	"encoding/json"
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
	runErr    error
	turnID    string
	ranResume bool
}

func (r *scriptRunner) RunTurn(ctx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	return agent.TurnResult{TurnID: r.turnID}, r.runErr
}
func (r *scriptRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	r.ranResume = true
	return agent.TurnResult{}, r.resumeErr
}

type scriptBuilder struct{ runner TurnRunner }

func (b *scriptBuilder) Build(ctx RuntimeContext) TurnRunner { return b.runner }

// terminalFailureBuilder emits the invocation-pairing event exactly as Runner
// does, then returns a terminal model error for TurnExecutor to publish as
// turn_failed. It exercises the persisted event boundary between both layers.
type terminalFailureBuilder struct{ err error }

func (b *terminalFailureBuilder) Build(ctx RuntimeContext) TurnRunner {
	return &terminalFailureRunner{pub: ctx.Publisher, err: b.err}
}

type terminalFailureRunner struct {
	pub agent.Emitter
	err error
}

func (r *terminalFailureRunner) RunTurn(_ context.Context, sess *session.Session, _ string) (agent.TurnResult, error) {
	r.pub.Emit(agent.Event{Kind: agent.EventModelFinished, SessionID: sess.ID, TurnID: "turn_42"})
	return agent.TurnResult{TurnID: "turn_42"}, r.err
}

func (r *terminalFailureRunner) ResumeTurn(context.Context, *session.Session) (agent.TurnResult, error) {
	return agent.TurnResult{}, nil
}

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

func TestFreshTurn401EmitsStructuredTurnFailed(t *testing.T) {
	repo := newFakeRepo()
	sess := &session.Session{
		ID:       "s1",
		Messages: []model.Message{{Role: model.RoleUser, Content: "check commit"}},
		Metadata: map[string]any{},
	}
	repo.sessions[sess.ID] = sess
	events := &fakeEventStore{}
	runner := &scriptRunner{
		runErr: &model.APIError{StatusCode: 401, Body: `{"msg":"missing token"}`},
		turnID: "turn_42",
	}
	ex := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), &scriptBuilder{runner: runner})

	if _, err := ex.ExecuteWithSession(context.Background(), sess, "check commit", ""); err == nil {
		t.Fatal("ExecuteWithSession succeeded, want model error")
	}
	if got := sess.TurnStatus(); got != session.TurnStatusFailed {
		t.Fatalf("status=%q want failed", got)
	}
	if len(events.records) != 2 {
		t.Fatalf("persisted events=%d want turn_accepted + turn_failed", len(events.records))
	}
	accepted, got := events.records[0], events.records[1]
	if accepted.Kind != string(agent.EventTurnAccepted) || got.Kind != string(agent.EventTurnFailed) || accepted.TurnID == "" || got.TurnID != accepted.TurnID {
		t.Fatalf("events = %q/%q and %q/%q, want same accepted/failed turn id", accepted.Kind, accepted.TurnID, got.Kind, got.TurnID)
	}
	var payload agent.Event
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if payload.ErrorCode != "auth_expired" || payload.Err == "" {
		t.Fatalf("failure payload = %+v, want auth_expired and message", payload)
	}
}

func TestLifecycleErrorCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"gateway quota", &model.APIError{StatusCode: 429, Code: "quota_exceeded"}, "quota_exceeded"},
		{"gateway quota type fallback", &model.APIError{StatusCode: 429, Type: "quota_exceeded"}, "quota_exceeded"},
		{"gateway auth", &model.APIError{StatusCode: 401}, "auth_expired"},
		{"generic", errors.New("boom"), "request_failed"},
	}
	for _, tc := range cases {
		if got := lifecycleErrorCode(tc.err); got != tc.want {
			t.Errorf("%s: lifecycleErrorCode() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestQuotaFailurePublishesOneDisplayError(t *testing.T) {
	repo := newFakeRepo()
	sess := &session.Session{
		ID:       "s1",
		Messages: []model.Message{{Role: model.RoleUser, Content: "check commit"}},
		Metadata: map[string]any{},
	}
	repo.sessions[sess.ID] = sess
	events := &fakeEventStore{}
	ex := NewTurnExecutor(
		repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(),
		&terminalFailureBuilder{err: &model.APIError{
			StatusCode: 429, Code: "quota_exceeded", Message: "daily allowance exhausted",
		}},
	)

	_, _ = ex.ExecuteWithSession(context.Background(), sess, "check commit", "")
	if len(events.records) != 3 {
		t.Fatalf("persisted events = %d, want turn_accepted + model_finished + turn_failed", len(events.records))
	}

	var modelFinished, turnFailed agent.Event
	for _, record := range events.records {
		var event agent.Event
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			t.Fatalf("decode %s: %v", record.Kind, err)
		}
		switch event.Kind {
		case agent.EventModelFinished:
			modelFinished = event
		case agent.EventTurnFailed:
			turnFailed = event
		}
	}
	if modelFinished.Err != "" {
		t.Fatalf("model_finished Err = %q, want empty", modelFinished.Err)
	}
	if turnFailed.Err == "" || turnFailed.ErrorCode != "quota_exceeded" {
		t.Fatalf("turn_failed = %+v, want quota error once", turnFailed)
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

// emitterFunc adapts a func to agent.Emitter.
type emitterFunc func(agent.Event)

func (f emitterFunc) Emit(e agent.Event) { f(e) }

// TestSequencingEmitterStampsLiveSeq is the v1.2 §4 invariant: a persisted event's
// live broadcast carries the same monotonic seq the store assigned, while an
// ephemeral token delta is forwarded live but neither persisted nor seq'd.
func TestSequencingEmitterStampsLiveSeq(t *testing.T) {
	events := &fakeEventStore{}
	var live []agent.Event
	se := &sequencingEmitter{
		ctx:    context.Background(),
		events: events,
		live:   emitterFunc(func(e agent.Event) { live = append(live, e) }),
	}

	se.Emit(agent.Event{Kind: agent.EventTurnStarted, SessionID: "s"})
	se.Emit(agent.Event{Kind: agent.EventThinking, SessionID: "s"})
	se.Emit(agent.Event{Kind: agent.EventTokenDelta, SessionID: "s"}) // ephemeral

	if len(live) != 3 {
		t.Fatalf("live got %d events, want 3", len(live))
	}
	if live[0].Seq != 1 || live[1].Seq != 2 {
		t.Errorf("live seqs = %d,%d want 1,2", live[0].Seq, live[1].Seq)
	}
	if live[2].Seq != 0 {
		t.Errorf("token_delta live seq = %d, want 0 (not persisted)", live[2].Seq)
	}
	if len(events.records) != 2 {
		t.Errorf("persisted %d events, want 2 (token_delta skipped)", len(events.records))
	}
}

// TestResume_NoOpWhenNotPaused is the bug-2 regression: resuming a session that is
// not paused (done/failed/normal) must NOT drive a turn — a host that calls resume
// on every foreground would otherwise re-invoke the model on a complete
// conversation and produce a spurious turn.
func TestResume_NoOpWhenNotPaused(t *testing.T) {
	repo := newFakeRepo()
	s := &session.Session{
		ID: "s1",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleUser, Content: "hi"},
		},
		Metadata: map[string]any{},
	}
	s.SetTurnStatus(session.TurnStatusDone)
	repo.sessions["s1"] = s
	runner := &scriptRunner{}
	ex := newExecutorWith(repo, runner)

	if _, err := ex.Resume(context.Background(), "s1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if runner.ranResume {
		t.Error("Resume drove a turn on a done session; it must no-op unless paused")
	}
	if got := repo.sessions["s1"].TurnStatus(); got != session.TurnStatusDone {
		t.Errorf("status changed to %q; want unchanged done", got)
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
		_, _ = ex.ExecuteWithSession(context.Background(), sess, "hi", "")
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
