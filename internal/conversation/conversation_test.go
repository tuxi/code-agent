package conversation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

// fakeRunner implements turnRunner without touching a model.
type fakeRunner struct {
	started   chan struct{} // RunTurn signals here once entered (buffered)
	block     chan struct{} // RunTurn waits on this when honorCtx is set
	honorCtx  bool
	ran       int
	lastInput string
	lastSess  *session.Session
}

func (f *fakeRunner) RunTurn(ctx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	f.ran++
	f.lastInput = input
	f.lastSess = sess
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.honorCtx {
		select {
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		case <-f.block:
			return agent.TurnResult{}, nil
		}
	}
	return agent.TurnResult{Final: "done"}, nil
}

type fakeStore struct {
	mu    sync.Mutex
	saves int
}

func (s *fakeStore) Save(ctx context.Context, sess *session.Session) error {
	s.mu.Lock()
	s.saves++
	s.mu.Unlock()
	return nil
}

func nonEmptySession() *session.Session {
	return &session.Session{ID: "s1", Messages: make([]model.Message, 2)}
}

// ctxStore records whether Save ran and whether it saw a canceled context — used
// to prove autosave uses context.WithoutCancel.
type ctxStore struct {
	saved       bool
	sawCanceled bool
	err         error
}

func (s *ctxStore) Save(ctx context.Context, sess *session.Session) error {
	s.saved = true
	if ctx.Err() != nil {
		s.sawCanceled = true
	}
	return s.err
}

func TestSendMessageRunsAndSaves(t *testing.T) {
	fr := &fakeRunner{}
	st := &fakeStore{}
	c := &Conversation{runner: fr, sess: nonEmptySession(), store: st, hub: newHub(nil)}

	res, err := c.SendMessage(context.Background(), "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.Final != "done" {
		t.Errorf("result = %q, want %q", res.Final, "done")
	}
	if fr.ran != 1 || fr.lastInput != "hello" {
		t.Errorf("RunTurn not driven correctly: ran=%d input=%q", fr.ran, fr.lastInput)
	}
	if st.saves != 1 {
		t.Errorf("want 1 autosave, got %d", st.saves)
	}
}

func TestSendMessageSkipsSaveForEmptySession(t *testing.T) {
	fr := &fakeRunner{}
	st := &fakeStore{}
	// One message (just the system prompt) => IsEmpty.
	sess := &session.Session{ID: "s1", Messages: make([]model.Message, 1)}
	c := &Conversation{runner: fr, sess: sess, store: st, hub: newHub(nil)}

	if _, err := c.SendMessage(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if st.saves != 0 {
		t.Errorf("empty session must not be saved, got %d", st.saves)
	}
}

func TestCancelStopsInFlightTurn(t *testing.T) {
	fr := &fakeRunner{started: make(chan struct{}, 1), block: make(chan struct{}), honorCtx: true}
	c := &Conversation{runner: fr, sess: nonEmptySession(), hub: newHub(nil)}

	done := make(chan error, 1)
	go func() {
		_, err := c.SendMessage(context.Background(), "x")
		done <- err
	}()

	<-fr.started // RunTurn has entered; c.cancel is set
	c.Cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendMessage did not return after Cancel")
	}
}

func TestSendMessageRejectsConcurrentTurn(t *testing.T) {
	fr := &fakeRunner{started: make(chan struct{}, 1), block: make(chan struct{}), honorCtx: true}
	c := &Conversation{runner: fr, sess: nonEmptySession(), hub: newHub(nil)}

	go func() { _, _ = c.SendMessage(context.Background(), "first") }()
	<-fr.started // first turn is now in flight

	if _, err := c.SendMessage(context.Background(), "second"); !errors.Is(err, ErrBusy) {
		t.Errorf("want ErrBusy for concurrent turn, got %v", err)
	}
	if fr.ran != 1 {
		t.Errorf("second turn should not have run RunTurn: ran=%d", fr.ran)
	}
	close(fr.block) // let the first turn finish
}

func TestAutosaveUsesWithoutCancel(t *testing.T) {
	st := &ctxStore{}
	c := &Conversation{runner: &fakeRunner{}, sess: nonEmptySession(), store: st, hub: newHub(nil)}

	// A canceled caller context must still autosave: WithoutCancel strips the
	// cancellation so the partial turn is persisted and resumable.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SendMessage(ctx, "x"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !st.saved {
		t.Error("autosave did not run under a canceled context")
	}
	if st.sawCanceled {
		t.Error("save saw a canceled context: WithoutCancel was not applied")
	}
}

func TestOnSaveErrorReportsFailure(t *testing.T) {
	st := &ctxStore{err: errors.New("disk full")}
	var got error
	c := &Conversation{runner: &fakeRunner{}, sess: nonEmptySession(), store: st, hub: newHub(nil)}
	c.OnSaveError = func(err error) { got = err }

	if _, err := c.SendMessage(context.Background(), "x"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got == nil || got.Error() != "disk full" {
		t.Errorf("OnSaveError not invoked with the save error, got %v", got)
	}
}

func TestSetSessionSwapsActiveSession(t *testing.T) {
	fr := &fakeRunner{}
	first := nonEmptySession()
	c := &Conversation{runner: fr, sess: first, hub: newHub(nil)}

	second := &session.Session{ID: "s2", Messages: make([]model.Message, 2)}
	c.SetSession(second)

	if c.ID() != "s2" {
		t.Errorf("ID() = %q, want s2", c.ID())
	}
	if _, err := c.SendMessage(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if fr.lastSess != second {
		t.Error("SendMessage drove the old session after SetSession")
	}
}

// TestNewInterceptsEmitter proves New replaces the Runner's Emitter with the hub
// while preserving the original as a downstream sink, so events reach both
// persistence and live subscribers.
func TestNewInterceptsEmitter(t *testing.T) {
	down := &recEmitter{}
	r := &agent.Runner{Emitter: down}
	c := New(r, &session.Session{ID: "s1"}, nil)

	ch, _ := c.Subscribe()
	r.Emitter.Emit(agent.Event{Kind: agent.EventTurnFinished, Text: "z"})

	if len(down.got) != 1 || down.got[0].Text != "z" {
		t.Errorf("downstream emitter not preserved: %+v", down.got)
	}
	select {
	case got := <-ch:
		if got.Text != "z" {
			t.Errorf("subscriber wrong event: %+v", got)
		}
	default:
		t.Error("subscriber received no event")
	}
}
