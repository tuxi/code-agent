package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

type backgroundProbe struct{}

func (backgroundProbe) Name() string                 { return "wait_probe" }
func (backgroundProbe) Description() string          { return "wait for runtime inputs" }
func (backgroundProbe) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (backgroundProbe) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "waiting", RuntimeWaitFor: []string{"job-a", "job-b"}}, nil
}

type backgroundProvider struct {
	mu            sync.Mutex
	calls         int
	blockResume   bool
	resumeStarted chan struct{}
}

func (p *backgroundProvider) Complete(ctx context.Context, _ model.Request) (model.Response, error) {
	p.mu.Lock()
	p.calls++
	call, block, started := p.calls, p.blockResume, p.resumeStarted
	p.mu.Unlock()
	if call > 1 && block {
		close(started)
		<-ctx.Done()
		return model.Response{}, ctx.Err()
	}
	if call == 1 {
		return model.Response{ToolCalls: []model.ToolCall{{ID: "call", Type: "function", Function: model.FunctionCall{Name: "wait_probe", Arguments: `{}`}}}, FinishReason: "tool_calls"}, nil
	}
	return model.Response{Content: "completed", FinishReason: "stop"}, nil
}

func TestBackgroundContinuationDurableReadyRecovery(t *testing.T) {
	store, old, sess, b := backgroundHarness(t)
	res, err := old.ExecuteWithRequestID(context.Background(), sess.ID, "request", "start", "frozen", "")
	if err != nil {
		t.Fatal(err)
	}
	old.Shutdown()
	completeBackground(t, store, sess.ID, res.TurnID, "job-a")
	repo := NewSQLiteRepository(store, 128000, 90000, "wire", "", nil)
	e := NewTurnExecutor(repo, &StoreEventAdapter{Store: store}, NewActiveTurnRegistry(), NewSubscriptionManager(), b)
	defer e.Shutdown()
	e.SetApprover(sess.ID, runtimeInputApprover{})
	if count, err := e.RecoverTurnInputs(context.Background()); err != nil || count != 0 {
		t.Fatal("waiting work entered ordinary recovery", count, err)
	}
	if _, err = e.Resume(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if b.provider.count() != 1 {
		t.Fatal("legacy resume re-executed waiting work")
	}
	e.SetBackgroundContinuationSupport(sess.ID, true)
	waitBackground(t, store, sess.ID, "finished")
	if b.provider.count() != 2 {
		t.Fatal("durable ready recovery duplicated work")
	}
}

func TestBackgroundContinuationDisconnectIsNotUserStop(t *testing.T) {
	store, e, sess, b := backgroundHarness(t)
	res, err := e.ExecuteWithRequestID(context.Background(), sess.ID, "request", "start", "frozen", "")
	if err != nil {
		t.Fatal(err)
	}
	b.provider.mu.Lock()
	b.provider.blockResume = true
	b.provider.resumeStarted = make(chan struct{})
	started := b.provider.resumeStarted
	b.provider.mu.Unlock()
	completeBackground(t, store, sess.ID, res.TurnID, "job-a")
	e.NotifyBackgroundCompletion(sess.ID)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("continuation did not start")
	}
	e.SetBackgroundContinuationSupport(sess.ID, false)
	waitBackground(t, store, sess.ID, "needs_attention")
	records, err := store.SessionEvents(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	paused := 0
	for _, record := range records {
		if record.Kind == string(agent.EventTurnCancelled) {
			t.Fatal("disconnect forged user stop")
		}
		if record.Kind == string(agent.EventTurnPaused) {
			paused++
		}
	}
	if paused != 1 {
		t.Fatal("disconnect was not observable", paused)
	}
}

func TestBackgroundContinuationNewInputPreemptsQueuedWake(t *testing.T) {
	store, e, sess, b := backgroundHarness(t)
	res, err := e.ExecuteWithRequestID(context.Background(), sess.ID, "request", "start", "frozen", "")
	if err != nil {
		t.Fatal(err)
	}
	release, err := e.scheduler.Acquire(context.Background(), TurnScheduleRequest{SessionID: "other", TurnID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	completeBackground(t, store, sess.ID, res.TurnID, "job-a")
	e.NotifyBackgroundCompletion(sess.ID)
	deadline := time.Now().Add(time.Second)
	for e.scheduler.Snapshot().Queued == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.ExecuteWithRequestID(context.Background(), sess.ID, "new-request", "new input", "frozen", "")
		done <- err
	}()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, _ := store.Continuation(context.Background(), sess.ID)
		if c.TurnID != res.TurnID {
			break
		}
		time.Sleep(time.Millisecond)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new input did not acquire worker")
	}
	if b.provider.count() != 2 {
		t.Fatal("old queued wake executed")
	}
	records, err := store.SessionEvents(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Kind == string(agent.EventTurnBackgroundResumed) {
			t.Fatal("superseded wake resumed")
		}
	}
}

func TestBackgroundContinuationCannotSwitchCredentialSource(t *testing.T) {
	store, e, sess, b := backgroundHarness(t)
	res, err := e.ExecuteWithRequestID(context.Background(), sess.ID, "request", "start", "frozen", "")
	if err != nil {
		t.Fatal(err)
	}
	// Even a non-nil resolver with no readable identity must not replace the
	// original default credential channel on an automatic continuation.
	e.SetSessionCredential(sess.ID, credential.StaticResolver{})
	completeBackground(t, store, sess.ID, res.TurnID, "job-a")
	e.NotifyBackgroundCompletion(sess.ID)
	waitBackground(t, store, sess.ID, "needs_attention")
	if b.provider.count() != 1 {
		t.Fatal("credential source changed during automatic resume")
	}
}

type failedBackgroundAdmission struct{ session.ContinuationStore }

func (f failedBackgroundAdmission) ClaimContinuation(context.Context, session.Continuation) (bool, error) {
	return false, errors.New("fixture admission storage failure")
}

func TestBackgroundContinuationStorageFailureIsObservable(t *testing.T) {
	store, e, sess, b := backgroundHarness(t)
	res, err := e.ExecuteWithRequestID(context.Background(), sess.ID, "request", "start", "frozen", "")
	if err != nil {
		t.Fatal(err)
	}
	completeBackground(t, store, sess.ID, res.TurnID, "job-a")
	c, err := store.Continuation(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	e.OnSaveError = func(err error) { reported = append(reported, err) }
	e.resumeBackground(context.Background(), failedBackgroundAdmission{store}, c)
	if len(reported) != 1 || b.provider.count() != 1 {
		t.Fatal("admission failure was silent or ran model", reported, b.provider.count())
	}
	waitBackground(t, store, sess.ID, "needs_attention")
}
func (p *backgroundProvider) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

type backgroundBuilder struct {
	provider *backgroundProvider
	identity string
}

func (b *backgroundBuilder) ResolveModel(string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: "frozen", Model: "wire"}, nil
}
func (b *backgroundBuilder) Build(c RuntimeContext) TurnRunner {
	reg := tools.NewRegistry()
	_ = reg.Register(backgroundProbe{})
	return &agent.Runner{Model: b.provider, ModelName: "wire", WireModel: "frozen", ModelSelection: model.Selection{ConfigurationID: b.identity}, WorkspaceRoot: c.Session.WorkspacePath, MaxSteps: 5, Tools: reg, RuntimeInputs: c.RuntimeInputs, ParkContinuation: c.ParkContinuation, ReservedTurnID: c.TurnID, RequestID: c.RequestID, Approver: c.Approver, Emitter: c.Publisher, Checkpointer: c.Checkpointer}
}

func backgroundHarness(t *testing.T) (*sqlite.Store, *TurnExecutor, *session.Session, *backgroundBuilder) {
	t.Helper()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo := NewSQLiteRepository(store, 128000, 90000, "wire", "", nil)
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	b := &backgroundBuilder{provider: &backgroundProvider{}, identity: "configuration"}
	e := NewTurnExecutor(repo, &StoreEventAdapter{Store: store}, NewActiveTurnRegistry(), NewSubscriptionManager(), b)
	e.SetApprover(sess.ID, runtimeInputApprover{})
	e.SetBackgroundContinuationSupport(sess.ID, true)
	t.Cleanup(func() {
		e.Shutdown()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			e.backgroundMu.Lock()
			n := len(e.backgroundAttempts)
			e.backgroundMu.Unlock()
			if n == 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_ = store.Close()
	})
	return store, e, sess, b
}

func completeBackground(t *testing.T, store *sqlite.Store, sid, tid, id string) {
	t.Helper()
	_, _, err := store.RecordRuntimeInput(context.Background(), session.RuntimeInput{ID: id, SessionID: sid, SourceTurnID: tid, SourceCallID: "call", Kind: "background_job_completion", Content: `{"status":"exited"}`, CreatedAt: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func waitBackground(t *testing.T, store *sqlite.Store, sid, state string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		c, err := store.Continuation(context.Background(), sid)
		if err == nil && c.State == state {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	c, err := store.Continuation(context.Background(), sid)
	t.Fatalf("waiting for %s: %+v %v", state, c, err)
}

func TestBackgroundContinuationExecutorReleasesSlotAndResumesOnce(t *testing.T) {
	store, e, sess, b := backgroundHarness(t)
	res, err := e.ExecuteWithRequestID(context.Background(), sess.ID, "request", "start", "frozen", "")
	if err != nil || !res.WaitingBackground {
		t.Fatal(res, err)
	}
	if snap := e.scheduler.Snapshot(); snap.Running != 0 || snap.Queued != 0 {
		t.Fatal("park held worker", snap)
	}
	input, err := store.TurnInput(context.Background(), sess.ID, "request")
	if err != nil || input.State != session.TurnInputWaitingBackground {
		t.Fatal(input, err)
	}
	completeBackground(t, store, sess.ID, res.TurnID, "job-a")
	completeBackground(t, store, sess.ID, res.TurnID, "job-b")
	for i := 0; i < 12; i++ {
		e.NotifyBackgroundCompletion(sess.ID)
	}
	waitBackground(t, store, sess.ID, "finished")
	if b.provider.count() != 2 {
		t.Fatal("duplicate model continuation", b.provider.count())
	}
	records, err := store.SessionEvents(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	starts, finishes, resumes := 0, 0, 0
	for _, record := range records {
		if record.TurnID != res.TurnID {
			t.Fatal("logical turn split", record)
		}
		switch record.Kind {
		case string(agent.EventTurnStarted):
			starts++
		case string(agent.EventTurnFinished):
			finishes++
		case string(agent.EventTurnBackgroundResumed):
			resumes++
		}
	}
	if starts != 1 || finishes != 1 || resumes != 1 {
		t.Fatal("invalid lifecycle", starts, finishes, resumes)
	}
}

func TestBackgroundContinuationStopAndNewUserPriority(t *testing.T) {
	for _, stop := range []bool{false, true} {
		store, e, sess, b := backgroundHarness(t)
		res, err := e.ExecuteWithRequestID(context.Background(), sess.ID, "request", "start", "frozen", "")
		if err != nil {
			t.Fatal(err)
		}
		if stop {
			if err = e.CancelTurn(context.Background(), sess.ID, res.TurnID); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err = e.ExecuteWithRequestID(context.Background(), sess.ID, "new-request", "new user input", "frozen", ""); err != nil {
				t.Fatal(err)
			}
			// A delayed old cancel must not touch the new work's marker.
			if err = e.CancelTurn(context.Background(), sess.ID, res.TurnID); err != nil {
				t.Fatal(err)
			}
		}
		completeBackground(t, store, sess.ID, res.TurnID, "job-a")
		e.NotifyBackgroundCompletion(sess.ID)
		e.backgroundMu.Lock()
		attempts := len(e.backgroundAttempts)
		e.backgroundMu.Unlock()
		want := 1
		if !stop {
			want = 2
		}
		if attempts != 0 || b.provider.count() != want {
			t.Fatal("stopped work resumed", attempts, b.provider.count())
		}
		c, err := store.Continuation(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stop && c.State != "stopped" {
			t.Fatal(c)
		}
		if stop {
			records, err := store.SessionEvents(context.Background(), sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			cancelled := 0
			for _, record := range records {
				if record.Kind == string(agent.EventTurnCancelled) {
					cancelled++
					var ev agent.Event
					if err = json.Unmarshal(record.Payload, &ev); err != nil || ev.ExecutedToolCalls != 1 {
						t.Fatal("cancel lost accounting", ev, err)
					}
				}
			}
			if cancelled != 1 {
				t.Fatal("duplicate or missing idle cancellation", cancelled)
			}
			loaded, err := store.Load(context.Background(), sess.ID)
			if err != nil || loaded.TurnStatus() != session.TurnStatusDone {
				t.Fatal("idle stop left waiting metadata", loaded, err)
			}
		}
		if !stop && c.TurnID == res.TurnID {
			t.Fatal("old work retained ownership")
		}
	}
}

func TestBackgroundContinuationQueuedStopAndConfigurationChange(t *testing.T) {
	store, e, sess, b := backgroundHarness(t)
	res, err := e.ExecuteWithRequestID(context.Background(), sess.ID, "request", "start", "frozen", "")
	if err != nil {
		t.Fatal(err)
	}
	block, err := e.scheduler.Acquire(context.Background(), TurnScheduleRequest{SessionID: "other", TurnID: "other-turn"})
	if err != nil {
		t.Fatal(err)
	}
	completeBackground(t, store, sess.ID, res.TurnID, "job-a")
	e.NotifyBackgroundCompletion(sess.ID)
	deadline := time.Now().Add(time.Second)
	for e.scheduler.Snapshot().Queued == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err = e.CancelTurn(context.Background(), sess.ID, res.TurnID); err != nil {
		t.Fatal(err)
	}
	block()
	waitBackground(t, store, sess.ID, "stopped")
	if b.provider.count() != 1 {
		t.Fatal("cancelled queue executed model")
	}
	records, err := store.SessionEvents(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := 0
	for _, record := range records {
		if record.Kind == string(agent.EventTurnCancelled) {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatal("queued stop lost terminal", cancelled)
	}
	// Fresh fixture: a configuration change fails closed before the model call.
	s2, e2, sess2, b2 := backgroundHarness(t)
	r2, err := e2.ExecuteWithRequestID(context.Background(), sess2.ID, "request", "start", "frozen", "")
	if err != nil {
		t.Fatal(err)
	}
	b2.identity = "changed"
	completeBackground(t, s2, sess2.ID, r2.TurnID, "job-a")
	e2.NotifyBackgroundCompletion(sess2.ID)
	waitBackground(t, s2, sess2.ID, "needs_attention")
	if b2.provider.count() != 1 {
		t.Fatal("configuration change invoked model")
	}
}
