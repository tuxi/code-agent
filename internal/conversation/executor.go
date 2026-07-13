package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/credential"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"sync"
)

// TurnExecutor is the single execution entry point for all transports (WS,
// HTTP, REPL, TUI, webhook, cron). It orchestrates one turn:
//
//	Load → BeginTurn → Build Runtime → Run → Save → FinishTurn
//
// It does NOT own the Runner — the Runtime is built per-turn and discarded.
// It does NOT build the Runner itself — that is RuntimeFactory's job.
type TurnExecutor struct {
	repo   ConversationRepository
	events ConversationEventStore
	active *ActiveTurnRegistry
	subs   *SubscriptionManager
	rb     RunBuilder

	// scheduler is the process-wide admission controller. Its default is a
	// single worker for backward-compatible FIFO behavior; deployments may raise
	// the limit only after advertising the matching runtime capabilities.
	scheduler *TurnScheduler

	// sessionCreds stores per-session credential resolvers, set by the handler
	// layer at WS upgrade time from the Authorization header. Server mode only;
	// embedded mode uses the injected StaticResolver instead.
	sessionCreds   map[string]credential.Resolver
	sessionCredsMu sync.RWMutex

	// titleGen, if set, asynchronously generates a human-readable title after the
	// first turn completes. Nil means no auto-titling (tests, headless).
	titleGen TitleGenerator

	// OnSaveError, if set, is invoked when the post-turn save fails. Autosave
	// is best-effort and never fails the turn; this only surfaces the warning
	// (the REPL prints it, a server logs it). Default nil = silent.
	OnSaveError func(error)
}

// SetTitleGenerator configures optional auto-titling. Separate from the
// constructor to keep the mandatory dependencies clear.
func (e *TurnExecutor) SetTitleGenerator(g TitleGenerator) {
	e.titleGen = g
}

// NewTurnExecutor wires the execution pipeline.
func NewTurnExecutor(repo ConversationRepository, events ConversationEventStore, active *ActiveTurnRegistry, subs *SubscriptionManager, rb RunBuilder) *TurnExecutor {
	return &TurnExecutor{
		repo:         repo,
		events:       events,
		active:       active,
		subs:         subs,
		rb:           rb,
		scheduler:    NewTurnScheduler(1),
		sessionCreds: make(map[string]credential.Resolver),
	}
}

// SetTurnScheduler replaces the admission controller. A nil value restores the
// conservative one-at-a-time scheduler. It is intended for daemon wiring at
// startup, before turns are accepted.
func (e *TurnExecutor) SetTurnScheduler(s *TurnScheduler) {
	if s == nil {
		s = NewTurnScheduler(1)
	}
	e.scheduler = s
}

// SetSessionCredential stores a credential resolver for a session. It is called
// by the handler layer at WS upgrade time after extracting the JWT from the
// Authorization header. In embedded mode this is never called — credential
// injection goes through secretsJSON instead.
func (e *TurnExecutor) SetSessionCredential(sessionID string, cred credential.Resolver) {
	e.sessionCredsMu.Lock()
	e.sessionCreds[sessionID] = cred
	e.sessionCredsMu.Unlock()
}

// sessionCredential returns the stored credential for a session, or nil.
func (e *TurnExecutor) sessionCredential(sessionID string) credential.Resolver {
	e.sessionCredsMu.RLock()
	defer e.sessionCredsMu.RUnlock()
	return e.sessionCreds[sessionID]
}

// Execute drives one turn to completion against the session identified by
// sessionID. It loads the session from the Repository, claims the turn slot,
// builds a fresh Runner, runs, saves, and releases the slot.
// model is the optional model profile name; empty means "use the server default".
func (e *TurnExecutor) Execute(ctx context.Context, sessionID string, input string, model string) (agent.TurnResult, error) {
	return e.ExecuteWithAssets(ctx, sessionID, input, model, nil)
}

// ExecuteWithAssets carries Gateway-owned user asset references into the next
// turn. The caller owns validation of their source; Gateway validates ownership
// again when it receives the chat request.
func (e *TurnExecutor) ExecuteWithAssets(ctx context.Context, sessionID string, input string, model string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	sess, err := e.repo.Load(ctx, sessionID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	return e.ExecuteWithSessionAssets(ctx, sess, input, model, assets)
}

// ExecuteWithSession runs a turn against an already-loaded session — the REPL
// and TUI path, where the caller holds a session handle across turns.
// model is the optional model profile name; empty means "use the server default".
func (e *TurnExecutor) ExecuteWithSession(parentCtx context.Context, sess *session.Session, input string, model string) (agent.TurnResult, error) {
	return e.ExecuteWithSessionAssets(parentCtx, sess, input, model, nil)
}

func (e *TurnExecutor) ExecuteWithSessionAssets(parentCtx context.Context, sess *session.Session, input string, model string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	res, runErr := e.driveTurn(parentCtx, sess, model,
		func(ctx context.Context, runner TurnRunner) (agent.TurnResult, error) {
			if assetRunner, ok := runner.(AssetTurnRunner); ok {
				return assetRunner.RunTurnWithAssets(ctx, sess, input, assets)
			}
			return runner.RunTurn(ctx, sess, input)
		},
		e.recordRunStatus,
	)

	// Auto-name: set an initial truncation-based name on the first turn, then fire
	// async LLM title generation if configured. Only the fresh-input path names;
	// a resume continues an already-named conversation.
	if sess.Name == "" {
		initial := truncateForTitle(firstUserMessage(sess))
		if initial != "" {
			sess.Name = initial
			go func() {
				_ = e.repo.UpdateName(context.WithoutCancel(parentCtx), sess.ID, initial)
			}()
		}
	}
	if e.titleGen != nil && turnCount(sess) == 1 {
		go e.generateTitleAsync(sess)
	}

	return res, runErr
}

// Resume continues an interrupted (paused) turn for a session, driving the agent
// loop from persisted history without appending a new user message (v1.2 §3.2).
// It loads the session, runs, and records the terminal turn_status (done on
// success, paused on a retryable failure, failed when unrecoverable / over the
// retry cap). Callers invoke it asynchronously — the embedded host's
// Server.ResumeSession launches it and observes progress over the event stream.
func (e *TurnExecutor) Resume(parentCtx context.Context, sessionID string) (agent.TurnResult, error) {
	sess, err := e.repo.Load(parentCtx, sessionID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	// Only a paused turn is resumable. A host that calls resume on every foreground
	// (the silent auto-resume pattern) will hit sessions that are done/failed/empty;
	// re-driving those would re-invoke the model on a complete conversation and
	// produce a spurious turn (e.g. the model answering the ephemeral skills
	// reminder). No-op unless there is genuinely an interrupted turn to continue.
	if sess.TurnStatus() != session.TurnStatusPaused {
		return agent.TurnResult{}, nil
	}
	return e.driveTurn(parentCtx, sess, "",
		func(ctx context.Context, runner TurnRunner) (agent.TurnResult, error) {
			return runner.ResumeTurn(ctx, sess)
		},
		e.recordResumeStatus,
	)
}

// driveTurn is the shared execution core for both a fresh turn (ExecuteWithSession)
// and a resume (Resume): claim the slot, assemble the publisher + runner, run the
// supplied driver, record the terminal lifecycle status, emit the matching
// lifecycle event, and save. The two paths differ only in the run closure
// (RunTurn vs ResumeTurn) and the status recorder.
func (e *TurnExecutor) driveTurn(
	parentCtx context.Context,
	sess *session.Session,
	model string,
	run func(ctx context.Context, runner TurnRunner) (agent.TurnResult, error),
	recordStatus func(sess *session.Session, runErr error),
) (agent.TurnResult, error) {
	// 1. Allocate the turn identity and publish acceptance before it can wait in
	// the scheduler. These events are persisted even for a cancelled queued turn,
	// so reconnecting clients see one complete lifecycle rather than a silent gap.
	turnID := e.scheduler.ReserveTurnID()
	pub := &sequencingEmitter{ctx: context.WithoutCancel(parentCtx), events: e.events, live: e.subs.Emitter(sess.ID)}
	pub.Emit(agent.Event{Kind: agent.EventTurnAccepted, SessionID: sess.ID, TurnID: turnID})

	// 2. Wait for the process/session/workspace execution permits before claiming
	// the active-turn slot. This makes a second turn in the same conversation a
	// queued operation rather than an ErrBusy race, and prevents concurrent
	// mutation of a shared checkout.
	release, err := e.scheduler.Acquire(parentCtx, TurnScheduleRequest{
		SessionID:     sess.ID,
		TurnID:        turnID,
		WorkspacePath: sess.WorkspacePath,
		Mode:          workspaceExecutionMode(sess.ExecutionPolicy()),
	}, func(position int) {
		pub.Emit(agent.Event{Kind: agent.EventTurnQueued, SessionID: sess.ID, TurnID: turnID, QueuePosition: position})
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			pub.Emit(agent.Event{Kind: agent.EventTurnCancelled, SessionID: sess.ID, TurnID: turnID})
		}
		return agent.TurnResult{}, err
	}
	defer release()

	// 3. Claim the turn (mutual exclusion). This remains the authority for
	// in-flight cancellation, approvals, and client-tool state.
	turnCtx, cancel, err := e.active.BeginTurn(sess.ID, parentCtx)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer func() {
		cancel()
		e.active.FinishTurn(sess.ID)
	}()

	// 4. Build a fresh turnRunner for this turn.
	rctx := RuntimeContext{
		Session:      sess,
		TurnID:       turnID,
		Model:        model,
		Publisher:    pub,
		Approver:     e.active.Approver(sess.ID),
		PlanApprover: e.active.PlanApprover(sess.ID),
		ClientWaiter: e.active.ClientToolWaiter(sess.ID),
		ClientTools:  e.active.ClientTools(sess.ID),
		Credential:   e.sessionCredential(sess.ID),
		// Mid-turn crash-safety (v1.2 §2): persist at each loop boundary so a hard
		// kill loses at most the in-progress step. The turn-boundary Save below is
		// still the backstop.
		Checkpointer: repoCheckpointer{repo: e.repo, onErr: e.OnSaveError},
	}
	runner := e.rb.Build(rctx)

	// 5. Run, marking running while in flight so a mid-turn crash leaves a
	//    detectable "interrupted" status (reconciled to paused on next start).
	sess.SetTurnStatus(session.TurnStatusRunning)
	res, runErr := run(turnCtx, runner)
	// The executor owns the accepted lifecycle identity. Alternate runners may
	// return a legacy/generated id, but it must never split one turn's event
	// stream into two correlations.
	res.TurnID = turnID

	// 6. Record the terminal status BEFORE the save, then emit the lifecycle event
	//    so a client sees paused/failed transitions.
	recordStatus(sess, runErr)
	e.emitLifecycle(pub, sess, res.TurnID, runErr)

	// 7. Save — always, even on error/cancel. The cancel-mid-batch fill + running
	//    marker keep the persisted history consistent and resumable. WithoutCancel
	//    keeps the save from being aborted by the turn's (or caller's) cancellation.
	if !sess.IsEmpty() {
		if serr := e.repo.Save(context.WithoutCancel(parentCtx), sess); serr != nil && e.OnSaveError != nil {
			e.OnSaveError(serr)
		}
	}

	return res, runErr
}

func workspaceExecutionMode(policy string) WorkspaceExecutionMode {
	switch policy {
	case session.ExecutionPolicyIsolatedWorktree:
		return IsolatedWorktree
	case session.ExecutionPolicyReadOnly:
		return ReadOnlyWorkspace
	default:
		return SharedWorkspace
	}
}

// maxResumeAttempts caps consecutive failed resumes of one session before it is
// declared failed, so a permanently-unresumable history is not retried forever
// (v1.2 §3.2.1).
const maxResumeAttempts = 5

// recordRunStatus sets the terminal turn_status for a fresh turn: done on success
// or user stop, paused only when the turn was cancelled by an app suspend (so the
// host can auto-continue it), and failed for every genuine runtime error.
func (e *TurnExecutor) recordRunStatus(sess *session.Session, runErr error) {
	switch {
	case runErr == nil:
		sess.SetTurnStatus(session.TurnStatusDone)
	case errors.Is(runErr, context.Canceled) && e.active.WasSuspended(sess.ID):
		sess.MarkPaused(time.Now())
	case errors.Is(runErr, context.Canceled):
		// Explicit cancel_turn is terminal on the client already and does not
		// require a lifecycle event from the server.
		sess.SetTurnStatus(session.TurnStatusDone)
	default:
		// A model/runtime failure must be observable as a terminal lifecycle
		// event; otherwise a client that has a running tool card never receives
		// the signal needed to stop its spinner.
		sess.SetTurnStatus(session.TurnStatusFailed)
	}
}

// recordResumeStatus classifies a resume outcome (v1.2 §3.2.1): success clears the
// attempt counter and marks done; a re-suspend goes back to paused untouched; a
// retryable failure re-pauses (incrementing attempts, escalating to failed past
// the cap); a non-retryable failure fails outright.
func (e *TurnExecutor) recordResumeStatus(sess *session.Session, runErr error) {
	switch {
	case runErr == nil:
		sess.SetTurnStatus(session.TurnStatusDone)
		sess.ClearResumeAttempts()
	case errors.Is(runErr, context.Canceled):
		// The resume was interrupted (re-suspended, or the server torn down), not a
		// genuine failure — stay paused for the next attempt, without counting it.
		sess.MarkPaused(time.Now())
	case model.IsRetryable(runErr):
		if sess.IncResumeAttempts() > maxResumeAttempts {
			sess.SetTurnStatus(session.TurnStatusFailed)
		} else {
			sess.MarkPaused(time.Now())
		}
	default:
		sess.SetTurnStatus(session.TurnStatusFailed)
	}
}

// emitLifecycle publishes the paused/failed lifecycle event matching the session's
// just-recorded terminal status, so a connected client updates its label. A done
// turn already emitted turn_finished from the loop, so nothing is published here.
func (e *TurnExecutor) emitLifecycle(pub agent.Emitter, sess *session.Session, turnID string, runErr error) {
	var kind agent.EventKind
	switch sess.TurnStatus() {
	case session.TurnStatusPaused:
		kind = agent.EventTurnPaused
	case session.TurnStatusFailed:
		kind = agent.EventTurnFailed
	default:
		return
	}
	event := agent.Event{Kind: kind, SessionID: sess.ID, TurnID: turnID, At: time.Now()}
	if kind == agent.EventTurnFailed {
		event.Err = errString(runErr)
		event.ErrorCode = lifecycleErrorCode(runErr)
	}
	pub.Emit(event)
}

func lifecycleErrorCode(err error) string {
	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Code == "quota_exceeded" || apiErr.Type == "quota_exceeded" {
			return "quota_exceeded"
		}
		if apiErr.StatusCode == 401 {
			return "auth_expired"
		}
	}
	return "request_failed"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// SuspendAll cancels every in-flight turn as an app suspend and awaits their
// unwind (bounded by ctx), returning the suspended session ids. The turns' own
// unwind records turn_status=paused. Called by the embedded host on background.
func (e *TurnExecutor) SuspendAll(ctx context.Context) []string {
	return e.active.SuspendAll(ctx)
}

// ReconcileInterrupted rewrites any session left mid-turn (turn_status running or
// resuming) to paused at startup: after a cold start nothing is actually running,
// so such a status marks a turn the process death interrupted. This unifies the
// hard-kill and clean-suspend paths — the host lists a single "paused" status to
// offer "continue" (v1.2 §3.2).
func (e *TurnExecutor) ReconcileInterrupted(ctx context.Context) error {
	metas, err := e.repo.List(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, m := range metas {
		if m.TurnStatus != session.TurnStatusRunning && m.TurnStatus != session.TurnStatusResuming {
			continue
		}
		sess, err := e.repo.Load(ctx, m.ID)
		if err != nil {
			continue // best-effort: skip a session that won't load
		}
		sess.MarkPaused(now)
		_ = e.repo.Save(ctx, sess)
	}
	return nil
}

// Cancel stops the in-flight turn for a session at the next checkpoint.
func (e *TurnExecutor) Cancel(sessionID string) {
	if e.scheduler.Cancel(sessionID) {
		return
	}
	e.active.Cancel(sessionID)
}

// Activity exposes the live scheduler projection for the HTTP activity
// endpoint. Persisted lifecycle state remains available through Repository.List.
func (e *TurnExecutor) Activity() []ScheduledTurnActivity {
	return e.scheduler.Activity()
}

// repoCheckpointer persists the session mid-turn via the repository (v1.2 §2). It
// detaches cancellation with WithoutCancel so a checkpoint fired as the turn is
// being suspended still commits, and skips an empty session (nothing to persist
// yet). Best-effort: it reports a failure through onErr but never blocks the turn —
// the turn-boundary Save in ExecuteWithSession is the authoritative write.
type repoCheckpointer struct {
	repo  ConversationRepository
	onErr func(error)
}

func (c repoCheckpointer) Checkpoint(ctx context.Context, sess *session.Session) error {
	if sess.IsEmpty() {
		return nil
	}
	err := c.repo.Save(context.WithoutCancel(ctx), sess)
	if err != nil && c.onErr != nil {
		c.onErr(err)
	}
	return err
}

// SetApprover associates an approver with a session.
func (e *TurnExecutor) SetApprover(sessionID string, a agent.Approver) {
	e.active.SetApprover(sessionID, a)
}

// SetPlanApprover associates a plan approver with a session.
func (e *TurnExecutor) SetPlanApprover(sessionID string, pa agent.PlanApprover) {
	e.active.SetPlanApprover(sessionID, pa)
}

// SetClientToolWaiter associates a client tool waiter with a session.
func (e *TurnExecutor) SetClientToolWaiter(sessionID string, w agent.ClientToolWaiter) {
	e.active.SetClientToolWaiter(sessionID, w)
}

// RegisterTools stores client-side tool definitions for a session.
func (e *TurnExecutor) RegisterTools(sessionID string, tools []agent.ClientToolDef) {
	e.active.RegisterTools(sessionID, tools)
}

// Shutdown cancels all active turns and closes event buses.
func (e *TurnExecutor) Shutdown() {
	e.scheduler.Shutdown()
	e.active.Shutdown()
	e.subs.Shutdown()
}

// ---- auto-naming helpers ----

// firstUserMessage returns the content of the first user message in the session.
func firstUserMessage(sess *session.Session) string {
	for _, m := range sess.Messages {
		if m.Role == model.RoleUser {
			return m.Content
		}
	}
	return ""
}

// truncateForTitle collapses whitespace and truncates msg to a reasonable
// display width for use as a fallback session title.
func truncateForTitle(msg string) string {
	msg = strings.TrimSpace(msg)
	// First line only.
	if idx := strings.IndexAny(msg, "\r\n"); idx >= 0 {
		msg = msg[:idx]
	}
	// Collapse whitespace.
	msg = strings.Join(strings.Fields(msg), " ")
	const maxLen = 60
	if len(msg) > maxLen {
		msg = msg[:maxLen]
	}
	return strings.TrimSpace(msg)
}

// turnCount returns the number of user turns in the session.
func turnCount(sess *session.Session) int {
	n := 0
	for _, m := range sess.Messages {
		if m.Role == model.RoleUser {
			n++
		}
	}
	return n
}

// generateTitleAsync runs the LLM title generator in a background goroutine.
// It uses a detached context with a timeout so it is not tied to the turn's
// lifecycle. Best-effort: failures are silently ignored.
func (e *TurnExecutor) generateTitleAsync(sess *session.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	userMsg := firstUserMessage(sess)
	assistantMsg := ""
	for _, m := range sess.Messages {
		if m.Role == model.RoleAssistant && m.Content != "" {
			assistantMsg = m.Content
			break
		}
	}

	title, err := e.titleGen.GenerateTitle(ctx, userMsg, assistantMsg)
	if err != nil || title == "" {
		return
	}
	_ = e.repo.UpdateName(ctx, sess.ID, title)
}

// ---- internal emitter ----

// sequencingEmitter persists each event to the ConversationEventStore — which
// assigns its monotonic seq — then stamps that seq onto the event and forwards it
// to the live subscriber sink, so a client's live seq is identical to the one the
// replay path (ReplaySince) will report (v1.2 §4). Persistence is best-effort: a
// marshal or write failure still forwards the event live (with seq 0) rather than
// dropping it. Ephemeral token deltas are not persisted (too frequent) and carry
// no seq; they are only forwarded live.
type sequencingEmitter struct {
	ctx    context.Context
	events ConversationEventStore
	live   agent.Emitter
}

func (s *sequencingEmitter) Emit(ev agent.Event) {
	if ev.Kind == agent.EventTokenDelta {
		s.live.Emit(ev)
		return
	}
	// Marshal BEFORE stamping seq: the persisted payload must not carry seq (that
	// lives in the rowid); replay re-stamps it from the row.
	if payload, err := json.Marshal(ev); err == nil {
		if seq, aerr := s.events.Append(s.ctx, session.EventRecord{
			SessionID: ev.SessionID,
			TurnID:    ev.TurnID,
			Kind:      string(ev.Kind),
			At:        ev.At,
			Payload:   payload,
		}); aerr == nil {
			ev.Seq = seq
		}
	}
	s.live.Emit(ev)
}
