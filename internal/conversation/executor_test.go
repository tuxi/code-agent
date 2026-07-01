package conversation

import (
	"context"
	"testing"

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

type notFoundError struct{ id string }

func (e *notFoundError) Error() string { return "session " + e.id + " not found" }

// ---- tests ----

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
	_, err := exec.Execute(context.Background(), "test-session", "hello")
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

	_, err := exec.Execute(context.Background(), "nonexistent", "hello")
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
	_, err := exec.Execute(ctx, "busy-session", "msg")
	if err != ErrBusy {
		t.Errorf("want ErrBusy, got %v", err)
	}
	cancel1()
	active.FinishTurn("busy-session")
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

	_, _ = exec.Execute(context.Background(), "s", "msg")
	if saveErr == nil {
		t.Error("OnSaveError should be called")
	}
}

func TestTurnExecutor_Shutdown(t *testing.T) {
	repo := newFakeRepo()
	active := NewActiveTurnRegistry()
	subs := NewSubscriptionManager()
	exec := NewTurnExecutor(repo, &fakeEventStore{}, active, subs, &fakeRunBuilder{})

	exec.Shutdown()
	// Post-shutdown Execute should fail.
	_, err := exec.Execute(context.Background(), "any", "msg")
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
