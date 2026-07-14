package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	requestMu     sync.Mutex
	requestClaims map[string]string // sessionID + NUL + requestID -> accepted turnID

	// sessionOps closes the TOCTOU gap between accepting a turn and deleting its
	// conversation. A turn holds a shared per-session claim from before Load until
	// its terminal save; deletion atomically rejects an existing claim and blocks
	// new claims until the repository delete has completed.
	sessionOpsMu sync.Mutex
	sessionOps   map[string]*sessionOperation

	// sessionCreds stores per-session credential resolvers, set by the handler
	// layer at WS upgrade time from the Authorization header. Server mode only;
	// embedded mode uses the injected StaticResolver instead.
	sessionCreds   map[string]credential.Resolver
	sessionCredsMu sync.RWMutex

	// titleGen, if set, asynchronously generates a human-readable title after the
	// first turn completes. Nil means no auto-titling (tests, headless).
	titleGen TitleGenerator

	// executionGuard optionally holds an external session resource lease for the
	// whole accepted/queued/running lifecycle. Managed worktrees use it to make
	// turn start and explicit removal atomic.
	executionGuard func(ctx context.Context, sessionID string) (release func(), err error)

	// OnSaveError, if set, is invoked when the post-turn save fails. Autosave
	// is best-effort and never fails the turn; this only surfaces the warning
	// (the REPL prints it, a server logs it). Default nil = silent.
	OnSaveError func(error)
}

type sessionOperation struct {
	turns    int
	deleting bool
}

// ErrConversationInUse is returned when a destructive conversation operation
// races with an accepted, queued, running, waiting, resuming, or paused turn.
var ErrConversationInUse = errors.New("conversation: conversation is in use")

// ErrConversationDeleting is returned to a turn submitted after deletion has
// atomically claimed the session. The turn is not accepted or persisted.
var ErrConversationDeleting = errors.New("conversation: conversation is being deleted")

// ConversationInUseError carries the stable state used by the HTTP layer while
// remaining compatible with errors.Is(err, ErrConversationInUse).
type ConversationInUseError struct {
	SessionID string
	State     string
}

func (e *ConversationInUseError) Error() string {
	if e.State == "" {
		return fmt.Sprintf("%s: %s", ErrConversationInUse, e.SessionID)
	}
	return fmt.Sprintf("%s: %s (%s)", ErrConversationInUse, e.SessionID, e.State)
}

func (e *ConversationInUseError) Unwrap() error { return ErrConversationInUse }

// SetTitleGenerator configures optional auto-titling. Separate from the
// constructor to keep the mandatory dependencies clear.
func (e *TurnExecutor) SetTitleGenerator(g TitleGenerator) {
	e.titleGen = g
}

// NewTurnExecutor wires the execution pipeline.
func NewTurnExecutor(repo ConversationRepository, events ConversationEventStore, active *ActiveTurnRegistry, subs *SubscriptionManager, rb RunBuilder) *TurnExecutor {
	return &TurnExecutor{
		repo:          repo,
		events:        events,
		active:        active,
		subs:          subs,
		rb:            rb,
		scheduler:     NewTurnScheduler(1),
		requestClaims: make(map[string]string),
		sessionOps:    make(map[string]*sessionOperation),
		sessionCreds:  make(map[string]credential.Resolver),
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

func (e *TurnExecutor) SetExecutionGuard(guard func(ctx context.Context, sessionID string) (func(), error)) {
	e.executionGuard = guard
}

func (e *TurnExecutor) beginSessionTurn(sessionID string) (func(), error) {
	e.sessionOpsMu.Lock()
	op := e.sessionOps[sessionID]
	if op == nil {
		op = &sessionOperation{}
		e.sessionOps[sessionID] = op
	}
	if op.deleting {
		e.sessionOpsMu.Unlock()
		return nil, ErrConversationDeleting
	}
	op.turns++
	e.sessionOpsMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.sessionOpsMu.Lock()
			op.turns--
			if op.turns == 0 && !op.deleting {
				delete(e.sessionOps, sessionID)
			}
			e.sessionOpsMu.Unlock()
		})
	}, nil
}

// DeleteConversation atomically excludes new turns, rejects every non-terminal
// lifecycle state, and deletes the durable session while the exclusion is held.
// The HTTP layer must only tear down connection-owned brokers after this returns
// nil; otherwise a rejected delete would incorrectly deny a pending approval.
func (e *TurnExecutor) DeleteConversation(ctx context.Context, sessionID string) error {
	e.sessionOpsMu.Lock()
	op := e.sessionOps[sessionID]
	if op == nil {
		op = &sessionOperation{}
		e.sessionOps[sessionID] = op
	}
	if op.deleting || op.turns > 0 {
		state := e.liveSessionState(sessionID)
		if state == "" {
			state = "accepted"
		}
		e.sessionOpsMu.Unlock()
		return &ConversationInUseError{SessionID: sessionID, State: state}
	}
	op.deleting = true
	e.sessionOpsMu.Unlock()

	defer func() {
		e.sessionOpsMu.Lock()
		op.deleting = false
		if op.turns == 0 {
			delete(e.sessionOps, sessionID)
		}
		e.sessionOpsMu.Unlock()
	}()

	metas, err := e.repo.List(ctx)
	if err != nil {
		return err
	}
	for _, meta := range metas {
		if meta.ID != sessionID {
			continue
		}
		if deletionBlockedTurnStatus(meta.TurnStatus) {
			return &ConversationInUseError{SessionID: sessionID, State: meta.TurnStatus}
		}
		break
	}
	return e.repo.Delete(ctx, sessionID)
}

func (e *TurnExecutor) liveSessionState(sessionID string) string {
	for _, activity := range e.scheduler.Activity() {
		if activity.SessionID == sessionID {
			return activity.State
		}
	}
	return ""
}

func deletionBlockedTurnStatus(status string) bool {
	switch status {
	case session.TurnStatusRunning, session.TurnStatusResuming, session.TurnStatusPaused:
		return true
	default:
		return false
	}
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

func (e *TurnExecutor) ExecuteWithRequestID(ctx context.Context, sessionID, requestID, input, modelName string) (agent.TurnResult, error) {
	return e.ExecuteWithRequestIDAndAssets(ctx, sessionID, requestID, input, modelName, nil)
}

// ExecuteWithAssets carries Gateway-owned user asset references into the next
// turn. The caller owns validation of their source; Gateway validates ownership
// again when it receives the chat request.
func (e *TurnExecutor) ExecuteWithAssets(ctx context.Context, sessionID string, input string, model string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	return e.ExecuteWithRequestIDAndAssets(ctx, sessionID, "", input, model, assets)
}

func (e *TurnExecutor) ExecuteWithRequestIDAndAssets(ctx context.Context, sessionID, requestID, input, modelName string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	release, err := e.beginSessionTurn(sessionID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer release()

	sess, err := e.repo.Load(ctx, sessionID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	return e.executeWithSessionAssets(ctx, sess, requestID, input, modelName, assets)
}

// ExecuteWithSession runs a turn against an already-loaded session — the REPL
// and TUI path, where the caller holds a session handle across turns.
// model is the optional model profile name; empty means "use the server default".
func (e *TurnExecutor) ExecuteWithSession(parentCtx context.Context, sess *session.Session, input string, model string) (agent.TurnResult, error) {
	return e.ExecuteWithSessionAssets(parentCtx, sess, input, model, nil)
}

func (e *TurnExecutor) ExecuteWithSessionAssets(parentCtx context.Context, sess *session.Session, input string, model string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	release, err := e.beginSessionTurn(sess.ID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer release()
	return e.executeWithSessionAssets(parentCtx, sess, "", input, model, assets)
}

func (e *TurnExecutor) executeWithSessionAssets(parentCtx context.Context, sess *session.Session, requestID, input, modelName string, assets []model.GatewayAssetRef) (agent.TurnResult, error) {
	res, runErr := e.driveTurn(parentCtx, sess, requestID, modelName,
		session.TurnStatusRunning,
		func(ctx context.Context, runner TurnRunner) (agent.TurnResult, error) {
			if assetRunner, ok := runner.(AssetTurnRunner); ok {
				return assetRunner.RunTurnWithAssets(ctx, sess, input, assets)
			}
			return runner.RunTurn(ctx, sess, input)
		},
		e.recordRunStatus,
	)
	if res.Deduplicated {
		return res, runErr
	}

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
	release, err := e.beginSessionTurn(sessionID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer release()

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
	return e.driveTurn(parentCtx, sess, "", "", session.TurnStatusResuming,
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
	requestID string,
	model string,
	runningState string,
	run func(ctx context.Context, runner TurnRunner) (agent.TurnResult, error),
	recordStatus func(sess *session.Session, runErr error),
) (agent.TurnResult, error) {
	// 1. Allocate the turn identity and publish acceptance before it can wait in
	// the scheduler. These events are persisted even for a cancelled queued turn,
	// so reconnecting clients see one complete lifecycle rather than a silent gap.
	turnID := e.scheduler.ReserveTurnID()
	pub := &sequencingEmitter{ctx: context.WithoutCancel(parentCtx), events: e.events, live: e.subs.Emitter(sess.ID)}
	if existingTurnID, duplicate := e.claimRequest(parentCtx, sess.ID, requestID, turnID, pub); duplicate {
		return agent.TurnResult{TurnID: existingTurnID, Deduplicated: true}, nil
	}
	if e.executionGuard != nil {
		releaseGuard, guardErr := e.executionGuard(parentCtx, sess.ID)
		if guardErr != nil {
			sess.SetTurnStatus(session.TurnStatusFailed)
			pub.Emit(agent.Event{
				Kind: agent.EventTurnFailed, SessionID: sess.ID, TurnID: turnID,
				Err: guardErr.Error(), ErrorCode: lifecycleErrorCode(guardErr), At: time.Now(),
			})
			_ = e.repo.Save(context.WithoutCancel(parentCtx), sess)
			if persistErr := pub.terminalPersistenceError(); persistErr != nil {
				guardErr = errors.Join(guardErr, persistErr)
			}
			return agent.TurnResult{TurnID: turnID}, guardErr
		}
		defer releaseGuard()
	}

	// 2. Wait for the process/session/workspace execution permits before claiming
	// the active-turn slot. This makes a second turn in the same conversation a
	// queued operation rather than an ErrBusy race, and prevents concurrent
	// mutation of a shared checkout.
	release, err := e.scheduler.Acquire(parentCtx, TurnScheduleRequest{
		SessionID:     sess.ID,
		TurnID:        turnID,
		WorkspacePath: sess.WorkspacePath,
		Mode:          workspaceExecutionMode(sess.ExecutionPolicy()),
		RunningState:  runningState,
	}, func(position int, reason TurnQueueReason) {
		pub.Emit(agent.Event{Kind: agent.EventTurnQueued, SessionID: sess.ID, TurnID: turnID, QueuePosition: position, QueueReason: string(reason)})
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			pub.Emit(agent.Event{Kind: agent.EventTurnCancelled, SessionID: sess.ID, TurnID: turnID})
			if persistErr := pub.terminalPersistenceError(); persistErr != nil {
				return agent.TurnResult{TurnID: turnID}, errors.Join(err, persistErr)
			}
		}
		return agent.TurnResult{TurnID: turnID}, err
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
	sess.SetTurnStatus(runningState)
	res, runErr := run(turnCtx, runner)
	// The executor owns the accepted lifecycle identity. Alternate runners may
	// return a legacy/generated id, but it must never split one turn's event
	// stream into two correlations.
	res.TurnID = turnID
	// turn_finished is emitted by the loop. If its durable append failed, the
	// turn cannot be reported as successfully complete; convert the outcome to a
	// visible executor failure before choosing the terminal session status.
	if persistErr := pub.terminalPersistenceError(); persistErr != nil {
		runErr = errors.Join(runErr, persistErr)
	}

	// 6. Record the terminal status BEFORE the save, then emit the lifecycle event
	//    so a client sees paused/failed transitions.
	recordStatus(sess, runErr)
	e.emitLifecycle(pub, sess, res.TurnID, runErr)
	if persistErr := pub.terminalPersistenceError(); persistErr != nil && !errors.Is(runErr, persistErr) {
		runErr = errors.Join(runErr, persistErr)
	}

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

func (e *TurnExecutor) claimRequest(ctx context.Context, sessionID, requestID, proposedTurnID string, pub agent.Emitter) (string, bool) {
	if requestID == "" {
		pub.Emit(agent.Event{Kind: agent.EventTurnAccepted, SessionID: sessionID, TurnID: proposedTurnID})
		return proposedTurnID, false
	}
	key := sessionID + "\x00" + requestID
	e.requestMu.Lock()
	defer e.requestMu.Unlock()
	if turnID := e.requestClaims[key]; turnID != "" {
		return turnID, true
	}
	if records, err := e.events.Replay(ctx, sessionID); err == nil {
		for _, record := range records {
			if record.Kind != string(agent.EventTurnAccepted) {
				continue
			}
			var event agent.Event
			if json.Unmarshal(record.Payload, &event) == nil && event.RequestID == requestID && event.TurnID != "" {
				e.requestClaims[key] = event.TurnID
				return event.TurnID, true
			}
		}
	}
	e.requestClaims[key] = proposedTurnID
	pub.Emit(agent.Event{Kind: agent.EventTurnAccepted, SessionID: sessionID, TurnID: proposedTurnID, RequestID: requestID})
	return proposedTurnID, false
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
		// Explicit cancel_turn is terminal and emitLifecycle persists the matching
		// turn_cancelled fact.
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
	case errors.Is(runErr, context.Canceled) && e.active.WasSuspended(sess.ID):
		// App suspension remains resumable.
		sess.MarkPaused(time.Now())
	case errors.Is(runErr, context.Canceled):
		// An explicit stop while resuming has the same terminal cancellation
		// semantics as a fresh running turn.
		sess.SetTurnStatus(session.TurnStatusDone)
		sess.ClearResumeAttempts()
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

// emitLifecycle publishes the executor-owned paused/failed/cancelled lifecycle
// event matching the session's just-recorded status. A successful done turn
// already emitted turn_finished from the loop.
func (e *TurnExecutor) emitLifecycle(pub agent.Emitter, sess *session.Session, turnID string, runErr error) {
	var kind agent.EventKind
	switch sess.TurnStatus() {
	case session.TurnStatusPaused:
		kind = agent.EventTurnPaused
	case session.TurnStatusFailed:
		kind = agent.EventTurnFailed
	case session.TurnStatusDone:
		if errors.Is(runErr, context.Canceled) && !e.active.WasSuspended(sess.ID) {
			kind = agent.EventTurnCancelled
		} else {
			return
		}
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
	var coded interface{ LifecycleErrorCode() string }
	if errors.As(err, &coded) && coded.LifecycleErrorCode() != "" {
		return coded.LifecycleErrorCode()
	}
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

func (e *TurnExecutor) HasActivity(sessionID string) bool {
	for _, activity := range e.scheduler.Activity() {
		if activity.SessionID == sessionID {
			return true
		}
	}
	return false
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
// replay path (ReplaySince) will report (v1.2 §4). Non-terminal persistence
// remains best-effort. Terminal persistence is authoritative: a failed append is
// retained for the executor and the unsequenced terminal is not forwarded live.
// Ephemeral token deltas are not persisted and carry no seq.
type sequencingEmitter struct {
	ctx    context.Context
	events ConversationEventStore
	live   agent.Emitter

	mu          sync.Mutex
	terminalErr error
}

func (s *sequencingEmitter) Emit(ev agent.Event) {
	if ev.Kind == agent.EventTokenDelta {
		s.live.Emit(ev)
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	// Marshal BEFORE stamping seq: the persisted payload must not carry seq (that
	// lives in the rowid); replay re-stamps it from the row.
	payload, err := json.Marshal(ev)
	if err == nil {
		var seq int64
		seq, err = s.events.Append(s.ctx, session.EventRecord{
			SessionID: ev.SessionID,
			TurnID:    ev.TurnID,
			Kind:      string(ev.Kind),
			At:        ev.At,
			Payload:   payload,
		})
		if err == nil {
			ev.Seq = seq
		}
	}
	if err != nil && isTerminalLifecycleEvent(ev.Kind) {
		s.mu.Lock()
		if s.terminalErr == nil {
			s.terminalErr = fmt.Errorf("persist %s for session %s turn %s: %w", ev.Kind, ev.SessionID, ev.TurnID, err)
		}
		s.mu.Unlock()
		// A terminal event without a durable sequence cannot be advertised as a
		// reliable completion. The executor observes terminalErr and fails the run.
		return
	}
	s.live.Emit(ev)
}

func (s *sequencingEmitter) terminalPersistenceError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalErr
}

func isTerminalLifecycleEvent(kind agent.EventKind) bool {
	switch kind {
	case agent.EventTurnFinished, agent.EventTurnFailed, agent.EventTurnCancelled:
		return true
	default:
		return false
	}
}
