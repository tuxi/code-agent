package goal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

// Worker advances the goal by one turn. The default implementation wraps
// *agent.Runner; the Engine never touches the session directly — it asks the
// worker to advance and reads back the result.
type Worker interface {
	// RunTurn runs one pursuit turn. ctx may be hard-cancelled (Ctrl-C), in which
	// case it returns a context.Canceled error and the Engine settles to paused.
	// On any error it still returns the partial TurnResult, whose TokensUsed
	// reflects what was billed before the failure.
	RunTurn(ctx context.Context, g *Goal) (agent.TurnResult, error)
}

// Engine orchestrates the pursue loop (the renamed goal.Runner — avoids
// colliding with agent.Runner, which is the per-turn executor).
//
// Resume contract: the Engine holds NO durable state. Everything that must
// survive a resume lives on the (persisted) Goal, so a resumed pursuit is a
// fresh Engine + the loaded Goal. The pause flag is ephemeral by design.
//
// The wiring fields are unexported and set only by NewEngine, so the three-way
// session alias (the worker, the transcript, and persist must all share ONE
// *session.Session) cannot be mis-assembled by a caller. Tuning knobs stay
// exported.
type Engine struct {
	worker  Worker
	checker Checker
	trans   Transcript
	store   session.Store
	sess    *session.Session

	// StallLimit is how many consecutive non-empty no-progress turns trip blocked
	// (0 = default). MaxErrors is how many consecutive transient errors are
	// tolerated before erroring out (0 = default).
	StallLimit int
	MaxErrors  int

	// DiffFunc, when set, returns the current workspace git diff. The engine calls
	// it each turn before the judge so the checker sees every file change (not just
	// what the worker surfaced) — the anti-gaming signal (§9.3). Injected by the cmd
	// layer (which owns the workspace root + the read-only git_diff tool); nil = no
	// forced diff (judge sees surfaced evidence only).
	DiffFunc func(ctx context.Context) string

	pause atomic.Bool

	// mu guards snap: Pursue runs in its own goroutine and mutates the live Goal,
	// while the REPL's `/goal` status command reads concurrently. Rather than lock
	// every field write in the hot loop, Pursue publishes an immutable value copy
	// at each persist; Snapshot returns that copy.
	mu   sync.Mutex
	snap *Goal
}

// NewEngine wires an Engine so the session alias is correct by construction: the
// worker, the transcript, and persist all share the one sess passed here. nil
// arguments are rejected at the boundary rather than panicking deep in the loop.
func NewEngine(sess *session.Session, store session.Store, runner *agent.Runner, checker Checker) (*Engine, error) {
	if sess == nil || store == nil || runner == nil || checker == nil {
		return nil, errors.New("goal.NewEngine: sess, store, runner and checker are all required")
	}
	return &Engine{
		worker:  &agentWorker{runner: runner, sess: sess},
		checker: checker,
		trans:   NewTranscript(sess),
		store:   store,
		sess:    sess,
	}, nil
}

// Pause requests a graceful stop at the next turn boundary.
func (e *Engine) Pause() { e.pause.Store(true) }

// Snapshot returns a consistent value copy of the goal as of the last persisted
// boundary, safe to call from another goroutine while Pursue runs. ok is false
// before the first persist.
func (e *Engine) Snapshot() (Goal, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snap == nil {
		return Goal{}, false
	}
	return *e.snap, true
}

const (
	defaultStallLimit = 5
	defaultMaxErrors  = 3
	// defaultMaxTurns is the hard budget floor imposed when a goal sets no budget
	// at all — no unbounded runs (§11). The admission layer should also reject a
	// budget-less goal; this is the belt behind that suspenders.
	defaultMaxTurns = 50
)

func (e *Engine) stallLimit() int {
	if e.StallLimit > 0 {
		return e.StallLimit
	}
	return defaultStallLimit
}

func (e *Engine) maxErrors() int {
	if e.MaxErrors > 0 {
		return e.MaxErrors
	}
	return defaultMaxErrors
}

// Pursue advances until achieved / blocked / errored / over-budget / paused,
// persisting on every boundary. It only advances when nothing has asked it to
// stop (Codex "idle-only advance"): the pause flag and budget are checked at the
// turn boundary, never mid-tool. It tolerates transient provider/checker errors
// so an unattended run survives a network blip, but stops on a permanent error
// (auth/4xx) or once tolerance is exhausted.
func (e *Engine) Pursue(ctx context.Context, g *Goal) error {
	// Reset the ephemeral pause flag at entry so a resume (which may reuse an
	// Engine whose flag was left set) can't instantly re-pause itself.
	e.pause.Store(false)

	// Budget floor: if the goal set no ceiling at all, impose a hard default.
	if g.Budget == (Budget{}) {
		g.Budget.MaxTurns = defaultMaxTurns
	}

	priorWall := g.Spent.Wall // accumulate across resume — never overwrite
	start := time.Now()
	g.Status = StatusActive

	bumpWall := func() { g.Spent.Wall = priorWall + time.Since(start) }
	// settle records a terminal/boundary state and persists it. A persist failure
	// here is surfaced (we could not save the outcome).
	settle := func(s Status, note string) error {
		g.Status = s
		g.StatusNote = note // distinct from the CheckerNote gradient
		bumpWall()
		return e.persist(g)
	}

	consecErrors := 0

	for {
		if e.pause.Load() {
			return settle(StatusPaused, "paused at turn boundary")
		}
		if over, why := g.overBudget(); over {
			return settle(StatusBudgetLimited, why)
		}

		res, err := e.worker.RunTurn(ctx, g)
		// Account real spend even on error: TokensUsed reflects what was billed
		// before the turn failed, so the budget counter stays honest.
		g.Spent.Tokens += res.TokensUsed

		if errors.Is(err, context.Canceled) {
			return settle(StatusPaused, "interrupted") // hard interrupt → paused, state preserved
		}
		if err != nil {
			consecErrors++
			bumpWall()
			if perr := e.persist(g); perr != nil {
				return perr
			}
			// A worker turn that failed did NOT complete, so the worker must re-run
			// (unlike the checker, which can retry in place). Stop on a permanent
			// error, or once we've exhausted tolerance for transient ones.
			if isPermanent(err) || consecErrors > e.maxErrors() {
				return settle(StatusErrored, "worker error: "+err.Error())
			}
			continue
		}
		g.Spent.Turns++

		// Capture the full workspace diff before judging so the checker sees every
		// change, not only what the worker surfaced (anti-gaming, §9.3).
		if e.DiffFunc != nil {
			g.diff = e.DiffFunc(ctx)
		}

		chk, cerr := e.check(ctx, g) // retries the cheap judge in place; worker not re-run
		if cerr != nil {
			bumpWall()
			if perr := e.persist(g); perr != nil {
				return perr
			}
			return settle(StatusErrored, "checker error: "+cerr.Error())
		}
		consecErrors = 0
		g.CheckerNote = chk.Reason // the gradient (only the checker writes this)

		switch {
		case chk.Blocked:
			return settle(StatusBlocked, chk.Reason)
		case chk.Met:
			// §11.2: achieved → cleared + archive is product policy for the command
			// layer, kept out of the Engine.
			return settle(StatusAchieved, chk.Reason)
		}

		// No-progress heuristic, but ONLY on non-empty evidence: an empty-evidence
		// turn (pure read_file/edit_file/grep research) is "no evidence yet", not
		// "no progress", and must not accrue toward blocked.
		if ev := e.trans.Evidence(); ev != "" {
			if sig := fingerprint(ev); sig == g.stallSig {
				g.stallCount++
				if g.stallCount >= e.stallLimit() {
					return settle(StatusBlocked, fmt.Sprintf("no progress across %d turns", g.stallCount))
				}
			} else {
				g.stallSig, g.stallCount = sig, 0
			}
		}

		bumpWall()
		if err := e.persist(g); err != nil {
			return err
		}
	}
}

// check runs the judge, retrying it in place on a transient error WITHOUT
// re-running the (expensive) worker — the evidence is already on the session. It
// stops on a permanent error or once the retry budget is spent.
func (e *Engine) check(ctx context.Context, g *Goal) (CheckResult, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		r, err := e.checker.Check(ctx, g, e.trans)
		if err == nil {
			return r, nil
		}
		lastErr = err
		if isPermanent(err) || attempt >= e.maxErrors() {
			return CheckResult{}, lastErr
		}
		// transient: try the cheap judge again, same evidence.
	}
}

// isPermanent reports whether an error should stop the loop immediately. Only a
// non-retryable model API error (auth / bad request / context-too-large) counts;
// unknown or transport-level errors are treated as transient (tolerated, bounded
// by maxErrors) — an unattended run should survive an unclassified blip, not die
// on it.
func isPermanent(err error) bool {
	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		return !model.IsRetryable(err)
	}
	return false
}

// persist is the SOLE session writer for a pursuit: it folds the goal into the
// session and saves messages + goal in one write, closing the worker/engine
// double-write window. It always uses a background context — durability must not
// be cancelled by the pause/stop signal the loop's own ctx carries — and
// publishes a snapshot for concurrent status reads.
func (e *Engine) persist(g *Goal) error {
	g.UpdatedAt = time.Now()
	g.IntoSession(e.sess)
	if err := e.store.Save(context.Background(), e.sess); err != nil {
		return fmt.Errorf("goal: persist failed: %w", err)
	}
	e.mu.Lock()
	cp := *g
	e.snap = &cp
	e.mu.Unlock()
	return nil
}

// ── default Worker: wraps the real agent loop ──────────────────────────────

// agentWorker is the default Worker. It owns the per-turn prompt strategy: the
// first turn of the goal's whole life sends the Objective; every later turn
// (including the first turn after a resume) sends a continuation carrying the
// judge's last gradient. The choice is driven off the PERSISTED Goal (Spent.Turns),
// not an ephemeral counter, so it is resume-safe.
//
// The worker does NOT persist: the Engine is the sole writer. It shares the same
// *session.Session the Engine holds (guaranteed by NewEngine).
type agentWorker struct {
	runner *agent.Runner
	sess   *session.Session
}

func (w *agentWorker) RunTurn(ctx context.Context, g *Goal) (agent.TurnResult, error) {
	input := g.Objective
	if g.Spent.Turns > 0 {
		input = renderContinuation(g.Objective, g.CheckerNote)
	}
	return w.runner.RunTurn(ctx, w.sess, input)
}

func renderContinuation(objective, gradient string) string {
	if gradient == "" {
		return "继续朝目标推进:\n" + objective
	}
	return fmt.Sprintf("目标尚未达成:\n%s\n\n评判反馈(上轮为何未达成):\n%s\n\n请据此继续推进,优先消除上面指出的差距。", objective, gradient)
}
