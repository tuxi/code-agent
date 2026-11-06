package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

type backgroundRunner interface {
	ContinueBackground(context.Context, *session.Session, json.RawMessage) (agent.TurnResult, error)
	ContinuationIdentity() string
	CanContinueBackground() bool
}

type backgroundBinding struct {
	Workspace, RequestID, WireModel, ResolvedModel, Effort, Identity, CredentialScope string
	HadCredential                                                                     bool
}

type backgroundAttempt struct {
	sessionID, turnID string
	cancel            context.CancelFunc
}

func (e *TurnExecutor) continuationStore() session.ContinuationStore {
	if provider, ok := e.repo.(session.ContinuationProvider); ok {
		return provider.ContinuationStore()
	}
	return nil
}

// Hosts must only opt in after negotiating waiting-state and targeted cancel
// support, and revoke support on disconnect/suspend. No legacy client opts in.
func (e *TurnExecutor) SetBackgroundContinuationSupport(sid string, enabled bool) {
	e.backgroundMu.Lock()
	if e.backgroundSupport == nil {
		e.backgroundSupport = make(map[string]bool)
	}
	e.backgroundSupport[sid] = enabled
	if !enabled {
		for _, a := range e.backgroundAttempts {
			if a.sessionID == sid {
				a.cancel()
			}
		}
	}
	e.backgroundMu.Unlock()
	if enabled {
		e.NotifyBackgroundCompletion(sid)
	}
}

func (e *TurnExecutor) backgroundSupported(sid string) bool {
	e.backgroundMu.Lock()
	defer e.backgroundMu.Unlock()
	return !e.backgroundClosed && e.backgroundSupport[sid]
}

func (e *TurnExecutor) cancelBackgroundAttempts(sid string) {
	e.backgroundMu.Lock()
	defer e.backgroundMu.Unlock()
	for _, a := range e.backgroundAttempts {
		if a.sessionID == sid {
			a.cancel()
		}
	}
}

func (e *TurnExecutor) backgroundScope(ctx context.Context, sid string) string {
	if e.assetRelease != nil {
		return e.assetRelease.CredentialScope(ctx, e.sessionCredential(sid))
	}
	return ""
}

func (e *TurnExecutor) backgroundParker(store session.ContinuationStore, binding *backgroundBinding, turnID string, publisher agent.Emitter) agent.ContinuationParker {
	return func(ctx context.Context, sess *session.Session, await []string, checkpoint json.RawMessage) error {
		if binding.Identity == "" || !e.backgroundSupported(sess.ID) || e.active.Approver(sess.ID) == nil {
			return errors.New("background continuation connection unavailable")
		}
		body, err := json.Marshal(binding)
		if err != nil {
			return err
		}
		parked, err := store.ParkContinuation(ctx, sess, session.Continuation{SessionID: sess.ID, TurnID: turnID, Await: await, Binding: body, Checkpoint: checkpoint})
		if errors.Is(err, session.ErrContinuationStale) {
			return context.Canceled
		}
		if err == nil && publisher != nil {
			info := backgroundInfo(parked)
			info.State = "waiting"
			publisher.Emit(agent.Event{Kind: agent.EventTurnWaitingBackground, SessionID: sess.ID, TurnID: turnID, Background: info})
		}
		return err
	}
}

// Completion is only a wake hint. Durable state/revision and scheduler permits
// decide whether work can execute; a callback can never directly run a model.
func (e *TurnExecutor) NotifyBackgroundCompletion(sid string) {
	store := e.continuationStore()
	if store == nil || !e.backgroundSupported(sid) {
		return
	}
	c, err := store.Continuation(context.Background(), sid)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			e.backgroundStorageError(err)
		}
		return
	}
	if c.State != "ready" {
		return
	}
	key := fmt.Sprintf("%s:%s:%d", sid, c.TurnID, c.Revision)
	e.backgroundMu.Lock()
	if e.backgroundClosed || !e.backgroundSupport[sid] || e.backgroundAttempts[key] != nil {
		e.backgroundMu.Unlock()
		return
	}
	if e.backgroundAttempts == nil {
		e.backgroundAttempts = make(map[string]*backgroundAttempt)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.backgroundAttempts[key] = &backgroundAttempt{sid, c.TurnID, cancel}
	e.backgroundWorkers.Add(1)
	e.backgroundMu.Unlock()
	go func() {
		defer e.backgroundWorkers.Done()
		defer func() { cancel(); e.backgroundMu.Lock(); delete(e.backgroundAttempts, key); e.backgroundMu.Unlock() }()
		e.resumeBackground(ctx, store, c)
	}()
}

func (e *TurnExecutor) backgroundStorageError(err error) {
	if e.OnSaveError != nil {
		e.OnSaveError(err)
		return
	}
	fmt.Fprintf(os.Stderr, "[background-continuation] %v\n", err)
}

func (e *TurnExecutor) backgroundBlocked(store session.ContinuationStore, c session.Continuation, pub *sequencingEmitter, err error) {
	if changed, saveErr := store.EndContinuation(context.Background(), c.SessionID, c.TurnID, "needs_attention"); saveErr == nil && changed {
		info := backgroundInfo(c)
		info.State = "needs_attention"
		info.Reason = err.Error()
		pub.Emit(agent.Event{Kind: agent.EventTurnBackgroundBlocked, SessionID: c.SessionID, TurnID: c.TurnID, Text: err.Error(), Background: info})
	} else if saveErr != nil {
		e.backgroundStorageError(saveErr)
	}
}

func (e *TurnExecutor) resumeBackground(ctx context.Context, store session.ContinuationStore, c session.Continuation) {
	pub := &sequencingEmitter{ctx: context.WithoutCancel(ctx), events: e.events, live: e.subs.Emitter(c.SessionID), toolStreams: agent.NewToolStreamCapper()}
	var binding backgroundBinding
	if err := json.Unmarshal(c.Binding, &binding); err != nil {
		e.backgroundBlocked(store, c, pub, err)
		return
	}
	releaseOperation, err := e.beginSessionTurn(c.SessionID)
	if err != nil {
		e.backgroundBlocked(store, c, pub, err)
		return
	}
	defer releaseOperation()
	release, err := e.scheduler.Acquire(ctx, TurnScheduleRequest{SessionID: c.SessionID, TurnID: c.TurnID, WorkspacePath: binding.Workspace, RunningState: session.TurnStatusResuming})
	if err != nil {
		return
	}
	defer release()
	if err = ctx.Err(); err != nil {
		return
	}
	latest, err := store.Continuation(ctx, c.SessionID)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			e.backgroundStorageError(err)
		}
		return
	}
	if latest.TurnID != c.TurnID || latest.Revision != c.Revision || latest.State != "ready" {
		return
	}
	sess, err := e.repo.Load(ctx, c.SessionID)
	if err != nil {
		e.backgroundBlocked(store, c, pub, err)
		return
	}
	if !sess.ArchivedAt.IsZero() || sess.WorkspacePath != binding.Workspace || !e.backgroundSupported(c.SessionID) || e.active.Approver(c.SessionID) == nil {
		e.backgroundBlocked(store, c, pub, errors.New("background continuation requires its original workspace and an interactive connection"))
		return
	}
	if rebind, err := e.repo.NeedsRebind(ctx, c.SessionID); err != nil || rebind {
		e.backgroundBlocked(store, c, pub, errors.New("background continuation workspace is unavailable"))
		return
	}
	if binding.HadCredential != (e.sessionCredential(c.SessionID) != nil) || (binding.HadCredential && binding.CredentialScope == "") || binding.CredentialScope != e.backgroundScope(ctx, c.SessionID) {
		e.backgroundBlocked(store, c, pub, errors.New("background continuation credential scope changed"))
		return
	}
	if resolver, ok := e.rb.(ModelResolver); ok {
		mc, resolveErr := resolver.ResolveModel(binding.WireModel)
		if resolveErr != nil || mc == nil || mc.Model != binding.ResolvedModel {
			e.backgroundBlocked(store, c, pub, errors.New("background continuation model is unavailable or changed"))
			return
		}
	}
	if e.executionGuard != nil {
		guardRelease, guardErr := e.executionGuard(ctx, sess.ID)
		if guardErr != nil {
			e.backgroundBlocked(store, c, pub, guardErr)
			return
		}
		defer guardRelease()
	}
	provider, ok := e.repo.(session.RuntimeInputProvider)
	if !ok {
		e.backgroundBlocked(store, c, pub, errors.New("runtime input store unavailable"))
		return
	}
	rctx := RuntimeContext{Session: sess, TurnID: c.TurnID, RequestID: binding.RequestID, Publisher: pub, Model: binding.WireModel, ResolvedModel: binding.ResolvedModel, ReasoningEffort: binding.Effort,
		Approver: e.active.Approver(sess.ID), PlanApprover: e.active.PlanApprover(sess.ID), AskUserApprover: e.active.AskUserApprover(sess.ID), ClientWaiter: e.active.ClientToolWaiter(sess.ID), ClientTools: e.active.ClientTools(sess.ID), Credential: e.sessionCredential(sess.ID), RuntimeInputs: provider.RuntimeInputStore(), Checkpointer: repoCheckpointer{repo: e.repo, onErr: e.OnSaveError}}
	rctx.ParkContinuation = e.backgroundParker(store, &binding, c.TurnID, pub)
	runner, ok := e.rb.Build(rctx).(backgroundRunner)
	if !ok || !runner.CanContinueBackground() || runner.ContinuationIdentity() != binding.Identity {
		e.backgroundBlocked(store, c, pub, errors.New("background continuation runtime configuration changed"))
		return
	}
	claimed, err := store.ClaimContinuation(ctx, c)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		e.backgroundStorageError(err)
		e.backgroundBlocked(store, c, pub, errors.New("background continuation admission could not be persisted"))
		return
	}
	if !claimed {
		return
	}
	if ctx.Err() != nil {
		latest, readErr := store.Continuation(context.Background(), c.SessionID)
		if readErr == nil && latest.TurnID == c.TurnID && latest.State == "stopped" {
			result, decodeErr := agent.BackgroundCheckpointResult(c.Checkpoint)
			if decodeErr == nil {
				sess.SetTurnStatus(session.TurnStatusDone)
				e.emitLifecycle(pub, sess, result, context.Canceled)
				if err = e.repo.Save(context.Background(), sess); err != nil && e.OnSaveError != nil {
					e.OnSaveError(err)
				}
			}
		} else {
			e.backgroundBlocked(store, c, pub, errors.New("background continuation admission interrupted"))
		}
		return
	}
	runCtx, cancel, err := e.active.BeginTurnWithID(sess.ID, c.TurnID, ctx)
	if err != nil {
		e.backgroundBlocked(store, c, pub, err)
		return
	}
	defer func() { cancel(); e.active.FinishTurn(sess.ID) }()
	sess.SetTurnStatus(session.TurnStatusResuming)
	info := backgroundInfo(c)
	info.State = "resuming"
	pub.Emit(agent.Event{Kind: agent.EventTurnBackgroundResumed, SessionID: sess.ID, TurnID: c.TurnID, Background: info})
	res, runErr := runner.ContinueBackground(runCtx, sess, c.Checkpoint)
	if res.WaitingBackground && runErr == nil {
		defer e.NotifyBackgroundCompletion(sess.ID)
		return
	}
	res.TurnID = c.TurnID
	if persistErr := pub.terminalPersistenceError(); persistErr != nil {
		runErr = errors.Join(runErr, persistErr)
	}
	connectionInterrupted := errors.Is(runErr, context.Canceled) && !e.backgroundSupported(sess.ID)
	if connectionInterrupted {
		latest, readErr := store.Continuation(context.Background(), sess.ID)
		connectionInterrupted = readErr == nil && latest.TurnID == c.TurnID && latest.State != "stopped"
	}
	if connectionInterrupted {
		sess.MarkPaused(time.Now())
	} else {
		e.recordRunStatus(sess, runErr)
	}
	e.emitLifecycle(pub, sess, res, runErr)
	e.setTurnInputState(ctx, session.TurnInput{SessionID: sess.ID, RequestID: binding.RequestID}, turnInputTerminalState(sess, runErr))
	state := "finished"
	if connectionInterrupted || e.active.WasSuspended(sess.ID) {
		state = "needs_attention"
	}
	if errors.Is(runErr, context.Canceled) && !e.active.WasSuspended(sess.ID) && !connectionInterrupted {
		state = "stopped"
	}
	if _, err = store.EndContinuation(context.Background(), sess.ID, c.TurnID, state); err != nil && e.OnSaveError != nil {
		e.OnSaveError(err)
	}
	if err = e.repo.Save(context.WithoutCancel(ctx), sess); err != nil && e.OnSaveError != nil {
		e.OnSaveError(err)
	}
}

func backgroundInfo(c session.Continuation) *agent.BackgroundContinuationInfo {
	info := &agent.BackgroundContinuationInfo{TurnID: c.TurnID, State: c.State, Revision: c.Revision}
	if info.State == "active" {
		info.State = "resuming"
	}
	for _, id := range c.Await {
		if value, ok := strings.CutPrefix(id, "job:"); ok {
			if value, ok = strings.CutSuffix(value, ":finished"); ok {
				info.JobIDs = append(info.JobIDs, value)
			}
		}
	}
	return info
}

// CancelTurn records the stop fence before cancelling a matching active/queued
// attempt. A late stop for an older turn cannot stop a newer user request.
func (e *TurnExecutor) CancelTurn(ctx context.Context, sid, tid string) error {
	if tid == "" {
		return errors.New("target turn_id is required")
	}
	if store := e.continuationStore(); store != nil {
		changed, err := store.EndContinuation(ctx, sid, tid, "stopped")
		if err != nil {
			return err
		}
		if changed {
			e.backgroundMu.Lock()
			for _, a := range e.backgroundAttempts {
				if a.sessionID == sid && a.turnID == tid {
					a.cancel()
				}
			}
			e.backgroundMu.Unlock()
			// Waiting/ready cancellation is durable with the stop fence; fan its
			// existing row rather than writing another lifecycle event.
			pub := &sequencingEmitter{ctx: context.WithoutCancel(ctx), events: e.events, live: e.subs.Emitter(sid)}
			if records, err := e.events.Replay(ctx, sid); err == nil {
				for _, record := range records {
					if record.TurnID == tid && record.Kind == string(agent.EventTurnCancelled) {
						var ev agent.Event
						if json.Unmarshal(record.Payload, &ev) == nil {
							ev.Seq = record.Seq
							pub.EmitPersisted(ev)
						}
						break
					}
				}
			}
		}
	}
	if e.scheduler.CancelTurn(sid, tid) {
		return nil
	}
	e.active.CancelTarget(sid, tid)
	return nil
}
