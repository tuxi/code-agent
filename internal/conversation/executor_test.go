package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

// ---- fake implementations for testing ----

// fakeRepo is an in-memory ConversationRepository for TurnExecutor tests.
type fakeRepo struct {
	sessions map[string]*session.Session
	events   []session.EventRecord
	metas    []session.Meta // returned by List when set
	saves    int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{sessions: make(map[string]*session.Session)}
}

func (r *fakeRepo) Create(ctx context.Context, workspacePath, workspaceExtID string) (*session.Session, error) {
	s := &session.Session{ID: "new_id", WorkspacePath: workspacePath, Model: "test"}
	r.sessions[s.ID] = s
	return s, nil
}
func (r *fakeRepo) Rebind(ctx context.Context, id, absPath string) error     { return nil }
func (r *fakeRepo) NeedsRebind(ctx context.Context, id string) (bool, error) { return false, nil }
func (r *fakeRepo) Load(ctx context.Context, id string) (*session.Session, error) {
	s, ok := r.sessions[id]
	if !ok {
		return nil, &notFoundError{id}
	}
	return s, nil
}
func (r *fakeRepo) Save(ctx context.Context, s *session.Session) error {
	r.saves++
	r.sessions[s.ID] = s
	return nil
}
func (r *fakeRepo) List(ctx context.Context) ([]session.Meta, error) { return r.metas, nil }
func (r *fakeRepo) Delete(ctx context.Context, id string) error {
	delete(r.sessions, id)
	return nil
}
func (r *fakeRepo) UpdateName(ctx context.Context, id string, name string) error {
	s, ok := r.sessions[id]
	if !ok {
		return &notFoundError{id}
	}
	s.Name = name
	return nil
}
func (r *fakeRepo) Close() error { return nil }

// fakeEventStore captures events for test assertions.
type fakeEventStore struct {
	records []session.EventRecord
	seq     int64
}

func (s *fakeEventStore) Append(ctx context.Context, e session.EventRecord) (int64, error) {
	s.seq++
	e.Seq = s.seq
	s.records = append(s.records, e)
	return s.seq, nil
}
func (s *fakeEventStore) Replay(ctx context.Context, sessionID string) ([]session.EventRecord, error) {
	return s.records, nil
}
func (s *fakeEventStore) ReplaySince(ctx context.Context, sessionID string, sinceSeq int64) ([]session.EventRecord, error) {
	var out []session.EventRecord
	for _, r := range s.records {
		if r.Seq > sinceSeq {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeRunBuilder returns a stubRunner that records what it was given.
type fakeRunBuilder struct {
	lastCtx RuntimeContext
}

func (b *fakeRunBuilder) Build(ctx RuntimeContext) TurnRunner {
	b.lastCtx = ctx
	return &stubRunner{}
}

// stubRunner is a minimal turnRunner that records the call.
type stubRunner struct {
	lastInput string
}

func (s *stubRunner) RunTurn(ctx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	s.lastInput = input
	return agent.TurnResult{}, nil
}

func (s *stubRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	s.lastInput = "" // resume appends no user input
	return agent.TurnResult{}, nil
}

type executionGuardError struct{}

func (executionGuardError) Error() string              { return "managed checkout is missing" }
func (executionGuardError) LifecycleErrorCode() string { return "worktree_missing" }

type blockingDeleteRepo struct {
	*fakeRepo
	deleteStarted  chan struct{}
	continueDelete chan struct{}
}

func (r *blockingDeleteRepo) Delete(ctx context.Context, id string) error {
	close(r.deleteStarted)
	select {
	case <-r.continueDelete:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.fakeRepo.Delete(ctx, id)
}

func TestTurnExecutorDeleteConversationRejectsActiveAndPausedTurns(t *testing.T) {
	repo := newFakeRepo()
	running := &session.Session{ID: "running-delete", WorkspacePath: "/work/running-delete", Metadata: map[string]any{}}
	repo.sessions[running.ID] = running
	started := make(chan string, 1)
	release := make(chan struct{})
	executor := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &schedulerBlockingRunBuilder{started: started, release: release})

	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(context.Background(), running.ID, "work", "")
		done <- err
	}()
	assertReady(t, started)
	err := executor.DeleteConversation(context.Background(), running.ID)
	var inUse *ConversationInUseError
	if !errors.As(err, &inUse) || inUse.State != session.TurnStatusRunning {
		t.Fatalf("active delete error=%v detail=%+v", err, inUse)
	}
	if _, ok := repo.sessions[running.ID]; !ok {
		t.Fatal("active conversation was deleted")
	}
	close(release)
	if err := assertReady(t, done); err != nil {
		t.Fatal(err)
	}

	paused := &session.Session{ID: "paused-delete", Metadata: map[string]any{}}
	paused.SetTurnStatus(session.TurnStatusPaused)
	repo.sessions[paused.ID] = paused
	repo.metas = []session.Meta{{ID: paused.ID, TurnStatus: session.TurnStatusPaused}}
	err = executor.DeleteConversation(context.Background(), paused.ID)
	inUse = nil
	if !errors.As(err, &inUse) || inUse.State != session.TurnStatusPaused {
		t.Fatalf("paused delete error=%v detail=%+v", err, inUse)
	}
	if _, ok := repo.sessions[paused.ID]; !ok {
		t.Fatal("paused conversation was deleted")
	}
}

func TestTurnExecutorDeleteConversationExcludesNewTurnAtomically(t *testing.T) {
	base := newFakeRepo()
	base.sessions["delete-race"] = &session.Session{ID: "delete-race", WorkspacePath: "/work/delete-race", Metadata: map[string]any{}}
	repo := &blockingDeleteRepo{fakeRepo: base, deleteStarted: make(chan struct{}), continueDelete: make(chan struct{})}
	executor := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &fakeRunBuilder{})

	deleted := make(chan error, 1)
	go func() { deleted <- executor.DeleteConversation(context.Background(), "delete-race") }()
	assertReady(t, repo.deleteStarted)
	if _, err := executor.Execute(context.Background(), "delete-race", "must not run", ""); !errors.Is(err, ErrConversationDeleting) {
		t.Fatalf("turn racing with delete error=%v, want ErrConversationDeleting", err)
	}
	close(repo.continueDelete)
	if err := assertReady(t, deleted); err != nil {
		t.Fatal(err)
	}
	if _, ok := base.sessions["delete-race"]; ok {
		t.Fatal("conversation still exists after guarded delete")
	}
}

func TestTurnExecutorArchiveIsIdempotentAndBlocksArchivedTurns(t *testing.T) {
	store := session.NewMemoryStore()
	repo := NewSQLiteRepository(store, 128000, 90000, "test", "", nil)
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	executor := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &fakeRunBuilder{})

	const callers = 8
	timestamps := make(chan time.Time, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at, err := executor.ArchiveConversation(context.Background(), sess.ID)
			timestamps <- at
			errs <- err
		}()
	}
	wg.Wait()
	close(timestamps)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first time.Time
	for at := range timestamps {
		if first.IsZero() {
			first = at
		} else if !at.Equal(first) {
			t.Fatalf("idempotent archive timestamps differ: %s / %s", first, at)
		}
	}
	if _, err := executor.Execute(context.Background(), sess.ID, "must not run", ""); !errors.Is(err, ErrConversationArchived) {
		t.Fatalf("archived Execute error=%v", err)
	}
	if _, err := executor.ExecuteWithSession(context.Background(), sess, "stale handle", ""); !errors.Is(err, ErrConversationArchived) {
		t.Fatalf("archived stale-session Execute error=%v", err)
	}
	if err := executor.RestoreConversation(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := executor.RestoreConversation(context.Background(), sess.ID); err != nil {
		t.Fatalf("idempotent restore: %v", err)
	}
	if _, err := executor.Execute(context.Background(), sess.ID, "runs after restore", ""); err != nil {
		t.Fatalf("restored Execute: %v", err)
	}
}

func TestTurnExecutorArchiveRejectsActiveTurn(t *testing.T) {
	store := session.NewMemoryStore()
	repo := NewSQLiteRepository(store, 128000, 90000, "test", "", nil)
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	release := make(chan struct{})
	executor := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &schedulerBlockingRunBuilder{started: started, release: release})
	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(context.Background(), sess.ID, "work", "")
		done <- err
	}()
	assertReady(t, started)
	_, err = executor.ArchiveConversation(context.Background(), sess.ID)
	var inUse *ConversationInUseError
	if !errors.As(err, &inUse) || inUse.State != session.TurnStatusRunning {
		t.Fatalf("archive active error=%v detail=%+v", err, inUse)
	}
	close(release)
	if err := assertReady(t, done); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.ArchiveConversation(context.Background(), sess.ID); err != nil {
		t.Fatalf("archive after terminal: %v", err)
	}
}

func TestTurnExecutorExecutionGuardEmitsStructuredTerminalFailure(t *testing.T) {
	repo := newFakeRepo()
	sess := &session.Session{ID: "managed", WorkspacePath: "/missing", Metadata: map[string]any{}}
	repo.sessions[sess.ID] = sess
	events := &fakeEventStore{}
	executor := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), &fakeRunBuilder{})
	executor.SetExecutionGuard(func(context.Context, string) (func(), error) {
		return nil, executionGuardError{}
	})
	result, err := executor.Execute(context.Background(), sess.ID, "run", "")
	if err == nil || result.TurnID == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(events.records) != 2 || events.records[0].Kind != string(agent.EventTurnAccepted) || events.records[1].Kind != string(agent.EventTurnFailed) {
		t.Fatalf("events=%+v", events.records)
	}
	var failed agent.Event
	if err := json.Unmarshal(events.records[1].Payload, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.TurnID != result.TurnID || failed.ErrorCode != "worktree_missing" || failed.Err == "" {
		t.Fatalf("failed=%+v", failed)
	}
}

// ctxCheckingEventStore refuses Append on a canceled ctx, like a real SQLite
// store would.
type ctxCheckingEventStore struct {
	fakeEventStore
}

func (s *ctxCheckingEventStore) Append(ctx context.Context, e session.EventRecord) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.fakeEventStore.Append(ctx, e)
}

type failingTerminalEventStore struct {
	fakeEventStore
	failKind agent.EventKind
}

func (s *failingTerminalEventStore) Append(ctx context.Context, e session.EventRecord) (int64, error) {
	if e.Kind == string(s.failKind) {
		return 0, errors.New("event store unavailable")
	}
	return s.fakeEventStore.Append(ctx, e)
}

// emitAfterCancelBuilder builds a runner that cancels the turn's parent ctx
// mid-run (simulating the WS connection closing while the turn is in flight)
// and then emits an event through the publisher.
type emitAfterCancelBuilder struct {
	cancelParent context.CancelFunc
}

func (b *emitAfterCancelBuilder) Build(rc RuntimeContext) TurnRunner {
	return &emitAfterCancelRunner{pub: rc.Publisher, cancel: b.cancelParent}
}

type emitAfterCancelRunner struct {
	pub    agent.Emitter
	cancel context.CancelFunc
}

func (r *emitAfterCancelRunner) RunTurn(ctx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	r.cancel() // the connection that started the turn goes away
	r.pub.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: sess.ID})
	return agent.TurnResult{}, nil
}

func (r *emitAfterCancelRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	return agent.TurnResult{}, nil
}

// funcRunBuilder builds a runner that hands the RuntimeContext to a closure —
// for tests that need to drive the publisher mid-turn.
type funcRunBuilder struct {
	fn func(rc RuntimeContext)
}

func (b *funcRunBuilder) Build(rc RuntimeContext) TurnRunner {
	return &funcRunner{rc: rc, fn: b.fn}
}

type funcRunner struct {
	rc RuntimeContext
	fn func(rc RuntimeContext)
}

type schedulerBlockingRunBuilder struct {
	started chan string
	release <-chan struct{}
}

func (b *schedulerBlockingRunBuilder) Build(rc RuntimeContext) TurnRunner {
	return &schedulerBlockingRunner{sessionID: rc.Session.ID, started: b.started, release: b.release}
}

type schedulerBlockingRunner struct {
	sessionID string
	started   chan string
	release   <-chan struct{}
}

type requestCountingBuilder struct {
	builds atomic.Int32
}

func (b *requestCountingBuilder) Build(RuntimeContext) TurnRunner {
	b.builds.Add(1)
	return &stubRunner{}
}

func (r *schedulerBlockingRunner) RunTurn(ctx context.Context, _ *session.Session, _ string) (agent.TurnResult, error) {
	r.started <- r.sessionID
	select {
	case <-r.release:
		return agent.TurnResult{}, nil
	case <-ctx.Done():
		return agent.TurnResult{}, ctx.Err()
	}
}

func (r *schedulerBlockingRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	return r.RunTurn(ctx, sess, "")
}

func (r *funcRunner) RunTurn(ctx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	r.fn(r.rc)
	return agent.TurnResult{}, nil
}

func (r *funcRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	return agent.TurnResult{}, nil
}

type notFoundError struct{ id string }

func (e *notFoundError) Error() string { return "session " + e.id + " not found" }

// ---- tests ----

func TestTurnExecutorRepairInvalidToolCallTailPersistsImmediately(t *testing.T) {
	repo := newFakeRepo()
	exec := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &fakeRunBuilder{})
	sess := &session.Session{
		ID:       "repair-session",
		Metadata: map[string]any{},
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "system"},
			{Role: model.RoleUser, Content: "answer my question"},
			{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "bad-call",
					Type: "function",
					Function: model.FunctionCall{
						Name:      "load_skill",
						Arguments: `{": "review-agent-runtime-architecture"}`,
					},
				}},
			},
			{Role: model.RoleTool, ToolCallID: "bad-call", Content: "invalid arguments"},
		},
	}
	repo.sessions[sess.ID] = sess

	if err := exec.repairInvalidToolCallTail(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if repo.saves != 1 {
		t.Fatalf("save calls = %d, want 1", repo.saves)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
	if got := sess.Messages[len(sess.Messages)-1]; got.Role != model.RoleUser || got.Content != "answer my question" {
		t.Fatalf("repaired tail ended at %#v", got)
	}
	if index, err := repo.sessions[sess.ID].FirstInvalidToolCallIndex(); err != nil || index != -1 {
		t.Fatalf("persisted session remains invalid: index=%d err=%v", index, err)
	}
}

func TestTurnExecutorRepairValidHistoryDoesNotSave(t *testing.T) {
	repo := newFakeRepo()
	exec := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &fakeRunBuilder{})
	sess := &session.Session{
		ID: "valid-session",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "system"},
			{Role: model.RoleUser, Content: "hello"},
			{Role: model.RoleAssistant, Content: "hi"},
		},
	}

	if err := exec.repairInvalidToolCallTail(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if repo.saves != 0 {
		t.Fatalf("save calls = %d, want 0", repo.saves)
	}
}

func TestTurnExecutor_Execute_LoadsAndSaves(t *testing.T) {
	repo := newFakeRepo()
	events := &fakeEventStore{}
	active := NewActiveTurnRegistry()
	subs := NewSubscriptionManager()
	rb := &fakeRunBuilder{}

	exec := NewTurnExecutor(repo, events, active, subs, rb)

	// Pre-populate a session.
	sess := &session.Session{
		ID:            "test-session",
		WorkspacePath: "/tmp/test",
		Model:         "test-model",
		// Session.IsEmpty() returns true when len(Messages) <= 1.
		// Add two messages so the save path fires.
		Messages: nil, // stubRunner emits nothing; IsEmpty stays true — we check OnSaveError below
	}
	repo.sessions["test-session"] = sess

	// Subscribe a WS client.
	ch, unsub := subs.Subscribe("test-session")
	defer unsub()

	// Use a stub runner so we control the turn.
	// We can't easily replace the runner in TurnExecutor (it builds via RunBuilder),
	// so we test through Execute and verify side effects.
	_, err := exec.Execute(context.Background(), "test-session", "hello", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// After execute, session should still be in repo (it was saved).
	_, err = repo.Load(context.Background(), "test-session")
	if err != nil {
		t.Errorf("session should be saved: %v", err)
	}

	// No events (stub runner didn't emit), but the channel should still be open.
	select {
	case <-ch:
		// ok if an event arrived
	default:
		// ok if no events (stub runner doesn't emit)
	}
}

func TestTurnExecutor_Execute_MissingSession(t *testing.T) {
	repo := newFakeRepo()
	exec := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &fakeRunBuilder{})

	_, err := exec.Execute(context.Background(), "nonexistent", "hello", "")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestTurnExecutor_Execute_Concurrency(t *testing.T) {
	repo := newFakeRepo()
	events := &fakeEventStore{}
	active := NewActiveTurnRegistry()
	subs := NewSubscriptionManager()
	rb := &fakeRunBuilder{}

	exec := NewTurnExecutor(repo, events, active, subs, rb)

	sess := &session.Session{ID: "busy-session", WorkspacePath: "/tmp", Model: "m"}
	repo.sessions["busy-session"] = sess

	// The fakeRunBuilder creates a real Runner that has RunTurn, so the
	// first Execute will block inside RunTurn on a real runner. To avoid
	// needing a full agent stack, we use ExecuteWithSession with a session
	// and verify ErrBusy for the second call.

	// Use a blocking approach: start first turn, then try second.
	ctx := context.Background()
	_, cancel1, _ := active.BeginTurn("busy-session", ctx)
	// Don't finish — second Execute should get ErrBusy.
	_, err := exec.Execute(ctx, "busy-session", "msg", "")
	if err != ErrBusy {
		t.Errorf("want ErrBusy, got %v", err)
	}
	cancel1()
	active.FinishTurn("busy-session")
}

func TestTurnExecutor_QueuesSameWorkspaceAndCancelsQueuedTurn(t *testing.T) {
	repo := newFakeRepo()
	first := &session.Session{ID: "one", WorkspacePath: "/work/shared", Metadata: map[string]any{}}
	second := &session.Session{ID: "two", WorkspacePath: "/work/shared", Metadata: map[string]any{}}
	repo.sessions[first.ID] = first
	repo.sessions[second.ID] = second

	release := make(chan struct{})
	started := make(chan string, 2)
	exec := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), NewSubscriptionManager(), &schedulerBlockingRunBuilder{started: started, release: release})
	exec.SetTurnScheduler(NewTurnScheduler(2))

	firstDone := make(chan error, 1)
	go func() { _, err := exec.Execute(context.Background(), first.ID, "first", ""); firstDone <- err }()
	if got := assertReady(t, started); got != first.ID {
		t.Fatalf("first started session = %q", got)
	}

	secondDone := make(chan error, 1)
	go func() { _, err := exec.Execute(context.Background(), second.ID, "second", ""); secondDone <- err }()
	deadline := time.Now().Add(time.Second)
	for exec.scheduler.Snapshot().Queued != 1 {
		if time.Now().After(deadline) {
			t.Fatal("second turn was not queued behind shared workspace lease")
		}
		time.Sleep(time.Millisecond)
	}
	exec.Cancel(second.ID)
	if err := assertReady(t, secondDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued turn error = %v, want context.Canceled", err)
	}
	var secondEvents []agent.Event
	for _, record := range exec.events.(*fakeEventStore).records {
		if record.SessionID != second.ID {
			continue
		}
		var event agent.Event
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			t.Fatal(err)
		}
		secondEvents = append(secondEvents, event)
	}
	if len(secondEvents) != 3 || secondEvents[0].Kind != agent.EventTurnAccepted || secondEvents[1].Kind != agent.EventTurnQueued || secondEvents[2].Kind != agent.EventTurnCancelled || secondEvents[0].TurnID == "" || secondEvents[0].TurnID != secondEvents[1].TurnID || secondEvents[1].TurnID != secondEvents[2].TurnID {
		t.Fatalf("queued cancellation events = %#v", secondEvents)
	}
	if secondEvents[1].QueuePosition != 1 {
		t.Fatalf("queue position = %d, want 1", secondEvents[1].QueuePosition)
	}
	if secondEvents[1].QueueReason != string(QueueReasonWorkspaceLease) {
		t.Fatalf("queue reason = %q, want %q", secondEvents[1].QueueReason, QueueReasonWorkspaceLease)
	}
	close(release)
	if err := assertReady(t, firstDone); err != nil {
		t.Fatalf("first turn error = %v", err)
	}
}

func TestTurnExecutor_RunningCancelPersistsExactlyOneTerminal(t *testing.T) {
	repo := newFakeRepo()
	repo.sessions["running"] = &session.Session{ID: "running", WorkspacePath: "/work/running", Metadata: map[string]any{}}
	events := &fakeEventStore{}
	started := make(chan string, 1)
	release := make(chan struct{})
	exec := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), &schedulerBlockingRunBuilder{started: started, release: release})

	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), "running", "work", "")
		done <- err
	}()
	if got := assertReady(t, started); got != "running" {
		t.Fatalf("started=%q", got)
	}
	exec.Cancel("running")
	if err := assertReady(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v want context.Canceled", err)
	}
	exec.Cancel("running") // repeated stop after terminal is a no-op

	cancelled := 0
	for _, record := range events.records {
		if record.Kind == string(agent.EventTurnCancelled) {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatalf("turn_cancelled count=%d records=%+v", cancelled, events.records)
	}
}

func TestTurnExecutor_TerminalPersistenceFailureIsObservable(t *testing.T) {
	repo := newFakeRepo()
	repo.sessions["s"] = &session.Session{ID: "s", WorkspacePath: "/work/s", Metadata: map[string]any{}}
	events := &failingTerminalEventStore{failKind: agent.EventTurnFinished}
	subs := NewSubscriptionManager()
	stream, unsubscribe := subs.Subscribe("s")
	defer unsubscribe()
	builder := &funcRunBuilder{fn: func(rc RuntimeContext) {
		rc.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: rc.Session.ID, TurnID: rc.TurnID, Text: "done"})
	}}
	exec := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), subs, builder)

	_, err := exec.Execute(context.Background(), "s", "work", "")
	if err == nil || !strings.Contains(err.Error(), "persist turn_finished") {
		t.Fatalf("error=%v want terminal persistence failure", err)
	}
	if repo.sessions["s"].TurnStatus() != session.TurnStatusFailed {
		t.Fatalf("turn status=%q want failed", repo.sessions["s"].TurnStatus())
	}
	for _, record := range events.records {
		if record.Kind == string(agent.EventTurnFinished) {
			t.Fatalf("unpersisted turn_finished appeared in durable records: %+v", events.records)
		}
	}
	foundFailed := false
	for {
		select {
		case event := <-stream:
			if event.Kind == agent.EventTurnFinished && event.Seq == 0 {
				t.Fatal("unpersisted terminal was forwarded live")
			}
			if event.Kind == agent.EventTurnFailed {
				foundFailed = true
			}
		default:
			if !foundFailed {
				t.Fatal("persisted turn_failed was not forwarded live")
			}
			return
		}
	}
}

func TestTurnExecutor_RequestIDIsIdempotentAcrossRestart(t *testing.T) {
	repo := newFakeRepo()
	repo.sessions["s"] = &session.Session{ID: "s", WorkspacePath: "/work/s", Metadata: map[string]any{}}
	events := &fakeEventStore{}
	firstBuilder := &requestCountingBuilder{}
	firstExecutor := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), firstBuilder)

	first, err := firstExecutor.ExecuteWithRequestID(context.Background(), "s", "request-1", "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := firstExecutor.ExecuteWithRequestID(context.Background(), "s", "request-1", "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.TurnID == "" || duplicate.TurnID != first.TurnID || !duplicate.Deduplicated {
		t.Fatalf("first=%+v duplicate=%+v", first, duplicate)
	}
	if firstBuilder.builds.Load() != 1 {
		t.Fatalf("builds=%d want 1", firstBuilder.builds.Load())
	}

	restartedBuilder := &requestCountingBuilder{}
	restarted := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), restartedBuilder)
	afterRestart, err := restarted.ExecuteWithRequestID(context.Background(), "s", "request-1", "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.TurnID != first.TurnID || !afterRestart.Deduplicated || restartedBuilder.builds.Load() != 0 {
		t.Fatalf("after restart=%+v builds=%d", afterRestart, restartedBuilder.builds.Load())
	}

	accepted := 0
	for _, record := range events.records {
		if record.Kind != string(agent.EventTurnAccepted) {
			continue
		}
		accepted++
		var event agent.Event
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.RequestID != "request-1" || event.TurnID != first.TurnID {
			t.Fatalf("accepted event=%+v", event)
		}
	}
	if accepted != 1 {
		t.Fatalf("turn_accepted count=%d want 1", accepted)
	}
}

func TestTurnExecutor_ConcurrentDuplicateRequestIDBuildsOneTurn(t *testing.T) {
	repo := newFakeRepo()
	repo.sessions["s"] = &session.Session{ID: "s", WorkspacePath: "/work/s", Metadata: map[string]any{}}
	events := &fakeEventStore{}
	builder := &requestCountingBuilder{}
	executor := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), builder)

	start := make(chan struct{})
	results := make(chan agent.TurnResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := executor.ExecuteWithRequestID(context.Background(), "s", "same-request", "hello", "")
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var turnID string
	deduplicated := 0
	for result := range results {
		if turnID == "" {
			turnID = result.TurnID
		}
		if result.TurnID != turnID {
			t.Fatalf("turn ids differ: %q vs %q", turnID, result.TurnID)
		}
		if result.Deduplicated {
			deduplicated++
		}
	}
	if turnID == "" || deduplicated != 1 || builder.builds.Load() != 1 {
		t.Fatalf("turn=%q deduplicated=%d builds=%d", turnID, deduplicated, builder.builds.Load())
	}
}

func TestTurnExecutor_OnSaveError(t *testing.T) {
	// Use a repo that fails on Save.
	repo := &failingSaveRepo{fakeRepo: newFakeRepo()}
	events := &fakeEventStore{}
	active := NewActiveTurnRegistry()
	subs := NewSubscriptionManager()

	var saveErr error
	exec := NewTurnExecutor(repo, events, active, subs, &fakeRunBuilder{})
	exec.OnSaveError = func(err error) { saveErr = err }

	sess := &session.Session{ID: "s", WorkspacePath: "/tmp", Model: "m"}
	// Add messages so IsEmpty() is false (len > 1) — save will fire.
	sess.Messages = append(sess.Messages, model.Message{}, model.Message{})
	repo.sessions["s"] = sess

	_, _ = exec.Execute(context.Background(), "s", "msg", "")
	if saveErr == nil {
		t.Error("OnSaveError should be called")
	}
}

// Events emitted after the caller's ctx dies (the WS that started the turn
// closed — e.g. the user switched conversations) must still be persisted, or
// replay shows a tool started but never finished.
func TestTurnExecutor_PersistsEventsAfterCallerContextCanceled(t *testing.T) {
	repo := newFakeRepo()
	events := &ctxCheckingEventStore{}
	repo.sessions["s1"] = &session.Session{ID: "s1", Model: "test"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rb := &emitAfterCancelBuilder{cancelParent: cancel}
	exec := NewTurnExecutor(repo, events, NewActiveTurnRegistry(), NewSubscriptionManager(), rb)

	if _, err := exec.Execute(ctx, "s1", "hello", ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(events.records) != 2 {
		t.Fatalf("got %d persisted events, want accepted + event after caller cancellation", len(events.records))
	}
	if events.records[0].Kind != string(agent.EventTurnAccepted) || events.records[1].Kind != string(agent.EventTurnFinished) {
		t.Errorf("persisted kinds = %q, %q, want turn_accepted, turn_finished", events.records[0].Kind, events.records[1].Kind)
	}
}

// The user's switch-away-and-back scenario: a turn is in flight, its only
// subscriber disconnects (bus torn down) and a new one connects (fresh bus).
// Events the turn emits afterwards — e.g. tool_finished right after the user
// approves on the new connection — must reach the new subscriber live, not only
// via the next replay.
func TestTurnExecutor_LiveEventsReachResubscribedClient(t *testing.T) {
	repo := newFakeRepo()
	repo.sessions["s1"] = &session.Session{ID: "s1", Model: "test"}
	subs := NewSubscriptionManager()
	defer subs.Shutdown()

	_, unsub1 := subs.Subscribe("s1") // the connection that starts the turn

	var ch2 <-chan agent.Event
	rb := &funcRunBuilder{fn: func(rc RuntimeContext) {
		unsub1()                      // user switches away mid-turn
		ch2, _ = subs.Subscribe("s1") // ...and back on a new connection
		rc.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: "s1"})
	}}

	exec := NewTurnExecutor(repo, &fakeEventStore{}, NewActiveTurnRegistry(), subs, rb)
	if _, err := exec.Execute(context.Background(), "s1", "hello", ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	select {
	case e := <-ch2:
		if e.Kind != agent.EventTurnFinished {
			t.Errorf("kind = %q, want %q", e.Kind, agent.EventTurnFinished)
		}
	default:
		t.Error("resubscribed client missed a live event emitted mid-turn")
	}
}

func TestTurnExecutor_Shutdown(t *testing.T) {
	repo := newFakeRepo()
	active := NewActiveTurnRegistry()
	subs := NewSubscriptionManager()
	exec := NewTurnExecutor(repo, &fakeEventStore{}, active, subs, &fakeRunBuilder{})

	exec.Shutdown()
	// Post-shutdown Execute should fail.
	_, err := exec.Execute(context.Background(), "any", "msg", "")
	if err == nil {
		t.Error("Execute should fail after Shutdown")
	}
}

// ---- helpers ----

type failingSaveRepo struct {
	*fakeRepo
}

func (r *failingSaveRepo) Save(ctx context.Context, s *session.Session) error {
	return &notFoundError{"save failed"}
}
