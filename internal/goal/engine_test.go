package goal

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

// ── test doubles ───────────────────────────────────────────────────────────

type fakeWorker struct {
	fn func(n int) (agent.TurnResult, error)
	n  int
}

func (w *fakeWorker) RunTurn(_ context.Context, _ *Goal) (agent.TurnResult, error) {
	defer func() { w.n++ }()
	if w.fn == nil {
		return agent.TurnResult{}, nil
	}
	return w.fn(w.n)
}

type fakeChecker struct {
	fn func(n int) (CheckResult, error)
	n  int
}

func (c *fakeChecker) Check(_ context.Context, _ *Goal, _ Transcript) (CheckResult, error) {
	defer func() { c.n++ }()
	if c.fn == nil {
		return CheckResult{}, nil
	}
	return c.fn(c.n)
}

type fakeTrans struct{ ev string }

func (t fakeTrans) Evidence() string { return t.ev }

// newTestEngine builds a white-box Engine with injected fakes. It sets the
// unexported wiring fields directly (only possible from package goal), which is
// how tests bypass NewEngine's real worker/transcript.
func newTestEngine(t *testing.T, w Worker, c Checker, tr Transcript) (*Engine, *Goal) {
	t.Helper()
	store, err := session.NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	sess := &session.Session{ID: "s", Metadata: map[string]any{}}
	e := &Engine{worker: w, checker: c, trans: tr, store: store, sess: sess}
	return e, &Goal{SessionID: "s", Objective: "obj"}
}

// ── tests ──────────────────────────────────────────────────────────────────

// NewEngine rejects nil dependencies at the boundary instead of panicking later.
func TestNewEngineRejectsNil(t *testing.T) {
	if _, err := NewEngine(nil, nil, nil, nil); err == nil {
		t.Error("want error for nil deps, got nil")
	}
}

// A stale pause flag (e.g. an Engine reused across resume) must NOT make Pursue
// immediately re-pause: the flag is reset at entry.
func TestPauseFlagResetOnPursue(t *testing.T) {
	achieve := &fakeChecker{fn: func(int) (CheckResult, error) { return CheckResult{Met: true, Reason: "green"}, nil }}
	e, g := newTestEngine(t, &fakeWorker{}, achieve, fakeTrans{})
	e.Pause() // stale flag set before the run
	if err := e.Pursue(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusAchieved {
		t.Errorf("want achieved (pause should reset at entry), got %s", g.Status)
	}
}

// A goal with no budget at all must still stop: the Engine imposes a hard
// MaxTurns floor. Empty evidence here also exercises "empty doesn't stall".
func TestBudgetFloorImposed(t *testing.T) {
	neverMet := &fakeChecker{fn: func(int) (CheckResult, error) { return CheckResult{}, nil }}
	e, g := newTestEngine(t, &fakeWorker{}, neverMet, fakeTrans{}) // ev == "" → no stall
	if err := e.Pursue(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusBudgetLimited {
		t.Fatalf("want budget_limited, got %s", g.Status)
	}
	if g.Budget.MaxTurns != defaultMaxTurns {
		t.Errorf("budget floor not imposed: %+v", g.Budget)
	}
	if g.Spent.Turns != defaultMaxTurns {
		t.Errorf("want %d turns, got %d", defaultMaxTurns, g.Spent.Turns)
	}
}

// Transient provider errors are tolerated (the loop survives a blip), partial
// token spend is still accounted, and only completed turns count.
func TestTransientErrorTolerated(t *testing.T) {
	transient := &model.APIError{StatusCode: 503}
	w := &fakeWorker{fn: func(n int) (agent.TurnResult, error) {
		if n < 2 {
			return agent.TurnResult{TokensUsed: 10}, transient // billed before failing
		}
		return agent.TurnResult{TokensUsed: 100}, nil
	}}
	achieve := &fakeChecker{fn: func(int) (CheckResult, error) { return CheckResult{Met: true, Reason: "ok"}, nil }}
	e, g := newTestEngine(t, w, achieve, fakeTrans{})
	if err := e.Pursue(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusAchieved {
		t.Fatalf("want achieved after tolerating transient errors, got %s (%s)", g.Status, g.StatusNote)
	}
	if g.Spent.Tokens != 120 { // 10 + 10 (failed turns) + 100
		t.Errorf("want 120 tokens incl. failed turns, got %d", g.Spent.Tokens)
	}
	if g.Spent.Turns != 1 {
		t.Errorf("only completed turns count; want 1, got %d", g.Spent.Turns)
	}
}

// An unclassified (non-API) error is treated as transient and tolerated, not an
// instant kill — an unattended run should survive an unknown blip.
func TestUnknownErrorTolerated(t *testing.T) {
	w := &fakeWorker{fn: func(n int) (agent.TurnResult, error) {
		if n < 2 {
			return agent.TurnResult{}, errors.New("some weird one-off")
		}
		return agent.TurnResult{}, nil
	}}
	achieve := &fakeChecker{fn: func(int) (CheckResult, error) { return CheckResult{Met: true, Reason: "ok"}, nil }}
	e, g := newTestEngine(t, w, achieve, fakeTrans{})
	if err := e.Pursue(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusAchieved {
		t.Fatalf("want achieved (unknown error tolerated), got %s (%s)", g.Status, g.StatusNote)
	}
}

// A permanent error (4xx) stops immediately as StatusErrored (infra), distinct
// from blocked (task-level).
func TestPermanentErrorErrored(t *testing.T) {
	w := &fakeWorker{fn: func(int) (agent.TurnResult, error) {
		return agent.TurnResult{}, &model.APIError{StatusCode: 401}
	}}
	e, g := newTestEngine(t, w, &fakeChecker{}, fakeTrans{})
	if err := e.Pursue(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusErrored {
		t.Fatalf("want errored on permanent error, got %s", g.Status)
	}
	if g.Spent.Turns != 0 {
		t.Errorf("a failed first turn isn't a completed turn; want 0, got %d", g.Spent.Turns)
	}
}

// A transient checker error is retried IN PLACE — the expensive worker runs only
// once, not once per checker blip.
func TestCheckerRetriedWithoutRerunningWorker(t *testing.T) {
	w := &fakeWorker{}
	transient := &model.APIError{StatusCode: 503}
	c := &fakeChecker{fn: func(n int) (CheckResult, error) {
		if n < 2 {
			return CheckResult{}, transient
		}
		return CheckResult{Met: true, Reason: "ok"}, nil
	}}
	e, g := newTestEngine(t, w, c, fakeTrans{})
	if err := e.Pursue(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusAchieved {
		t.Fatalf("want achieved after in-place checker retry, got %s", g.Status)
	}
	if w.n != 1 {
		t.Errorf("worker should run exactly once despite checker retries, ran %d times", w.n)
	}
}

// Constant NON-empty evidence with a never-met checker trips the no-progress
// heuristic well before the budget.
func TestStallBlocksOnConstantEvidence(t *testing.T) {
	neverMet := &fakeChecker{fn: func(int) (CheckResult, error) { return CheckResult{}, nil }}
	e, g := newTestEngine(t, &fakeWorker{}, neverMet, fakeTrans{ev: "same output every time"})
	if err := e.Pursue(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusBlocked {
		t.Fatalf("want blocked via stall, got %s", g.Status)
	}
	if g.Spent.Turns >= defaultMaxTurns {
		t.Errorf("stall should trip before budget, ran %d turns", g.Spent.Turns)
	}
}

// Snapshot is safe to call concurrently with a running Pursue (run with -race).
func TestSnapshotConcurrentNoRace(t *testing.T) {
	neverMet := &fakeChecker{fn: func(int) (CheckResult, error) { return CheckResult{}, nil }}
	e, g := newTestEngine(t, &fakeWorker{}, neverMet, fakeTrans{})
	g.Budget = Budget{MaxTurns: 30}
	done := make(chan error, 1)
	go func() { done <- e.Pursue(context.Background(), g) }()
	for i := 0; i < 2000; i++ {
		_, _ = e.Snapshot()
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
