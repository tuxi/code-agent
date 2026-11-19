package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

type QueueExecutionStore interface {
	session.UserQueueDeliveryStore
	session.UserQueueAdmissionStore
	PauseUserQueueContext(context.Context, string) error
	StartUserQueueClaim(context.Context, session.TurnInput, *session.Session, session.QueueExecutionContext) (int64, error)
}

// QueueContextResolver is supplied by the trusted host. It must resolve current
// credentials, model availability/effort and workspace permissions, not echo a
// client snapshot. It is invoked again after waiting for scheduler capacity.
type QueueContextResolver func(context.Context, string, session.QueueExecutionContext) (session.QueueExecutionContext, error)
type queueExecutionKey struct{}
type queueExecution struct {
	frozen   session.QueueExecutionContext
	store    QueueExecutionStore
	resolve  QueueContextResolver
	resuming bool
}

func queueExecutionFrom(ctx context.Context) *queueExecution {
	v, _ := ctx.Value(queueExecutionKey{}).(*queueExecution)
	return v
}

func (e *TurnExecutor) ExecuteUserQueueClaim(ctx context.Context, sid, rid string, store QueueExecutionStore, resolve QueueContextResolver) (agent.TurnResult, error) {
	if store == nil || resolve == nil {
		return agent.TurnResult{}, errors.New("queue execution requires trusted context resolver")
	}
	consumerKey := "user-queue-consumer\x00" + sid
	if !e.claimRecovery(consumerKey) {
		return agent.TurnResult{}, session.ErrUserQueueUnavailable
	}
	defer e.releaseRecovery(consumerKey)
	advance, err := store.ReconcileUserQueue(ctx, sid)
	if err != nil {
		return agent.TurnResult{}, err
	}
	if advance.Claim == nil || advance.Claim.Message.Input.RequestID != rid {
		return agent.TurnResult{}, session.ErrUserQueueUnavailable
	}
	frozen := advance.Claim.Message.Input.Context
	current, err := resolve(ctx, sid, frozen)
	if err != nil {
		return agent.TurnResult{}, errors.Join(err, store.PauseUserQueueContext(context.WithoutCancel(ctx), sid))
	}
	input, _, _, err := store.ReserveUserQueueClaim(ctx, sid, rid, current)
	if err != nil {
		return agent.TurnResult{}, err
	}
	key := sid + "\x00" + input.RequestID
	if !e.claimRecovery(key) {
		return agent.TurnResult{}, session.ErrUserQueueUnavailable
	}
	defer e.releaseRecovery(key)
	result, runErr := e.executeRecoveredTurn(context.WithValue(ctx, queueExecutionKey{}, &queueExecution{frozen: frozen, store: store, resolve: resolve}), input)
	if runErr != nil {
		runErr = errors.Join(runErr, store.PauseUserQueueContext(context.WithoutCancel(ctx), sid))
	}
	return result, runErr
}

// Ordinary recovery must not run queued inputs with default credentials/effort.
func (e *TurnExecutor) isUserQueueInput(ctx context.Context, input session.TurnInput) (bool, error) {
	if !strings.HasPrefix(input.RequestID, "queue_") {
		return false, nil
	}
	records, err := e.events.Replay(ctx, input.SessionID)
	if err != nil {
		return false, err
	}
	for _, r := range records {
		if r.Seq == input.AcceptedSeq {
			var envelope map[string]json.RawMessage
			if err = json.Unmarshal(r.Payload, &envelope); err != nil {
				return false, err
			}
			_, found := envelope["queue_context"]
			return found, nil
		}
	}
	return false, nil
}

// DrainUserQueue is explicit and bounded; installing storage never starts it.
// Waiting for background work ends this drain, even though it is not a failure.
func (e *TurnExecutor) DrainUserQueue(ctx context.Context, sid string, store QueueExecutionStore, resolve QueueContextResolver, limit int) (int, error) {
	if limit < 1 || limit > 100 || store == nil || resolve == nil {
		return 0, session.ErrInvalidQueuedMessage
	}
	count := 0
	for count < limit {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		next, err := store.ReconcileUserQueue(ctx, sid)
		if err != nil {
			return count, err
		}
		if next.Claim == nil {
			return count, nil
		}
		result, err := e.ExecuteUserQueueClaim(ctx, sid, next.Claim.Message.Input.RequestID, store, resolve)
		if err != nil {
			return count, err
		}
		if result.WaitingBackground {
			return count, nil
		}
		count++
	}
	return count, nil
}

func (e *TurnExecutor) finishQueueExecution(ctx context.Context, sid string, input session.TurnInput, q *queueExecution) error {
	records, err := e.events.Replay(ctx, sid)
	if err != nil {
		return err
	}
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		if r.TurnID != input.TurnID {
			continue
		}
		var state session.TurnInputState
		switch r.Kind {
		case "turn_finished":
			state = session.TurnInputCompleted
		case "turn_failed":
			state = session.TurnInputFailed
		case "turn_cancelled":
			state = session.TurnInputCancelled
		default:
			continue
		}
		_, err = q.store.FinishUserQueueTurn(ctx, session.UserQueueTurnEnd{SessionID: sid, RequestID: input.RequestID, TurnID: input.TurnID, State: state, Event: r})
		return err
	}
	return errors.New("queue execution has no durable terminal event")
}

// ConfigureUserQueueWakeup installs a startup-only callback for durable
// ordinary turns. Wakeup runs after scheduler capacity is released.
func (e *TurnExecutor) ConfigureUserQueueWakeup(wake func(string)) { e.queueWake = wake }

// UsesPrivateQueueCredentials excludes independently injected provider identities
// from a queue explicitly scoped to the runtime owner's configured providers.
func (e *TurnExecutor) UsesPrivateQueueCredentials(sid string) bool {
	return e.sessionCredential(sid) == nil && e.active.ApprovalMode(sid) == ""
}

// ResumeInterruptedUserQueue requires a durable user-requested resume. It never
// appends another user message or re-admits the original input.
func (e *TurnExecutor) ResumeInterruptedUserQueue(ctx context.Context, sid string, store QueueExecutionStore, resolve QueueContextResolver) (agent.TurnResult, error) {
	recovery, ok := store.(session.InterruptedUserQueueStore)
	if !ok || resolve == nil {
		return agent.TurnResult{}, session.ErrUserQueueUnavailable
	}
	key := "user-queue-consumer\x00" + sid
	if !e.claimRecovery(key) {
		return agent.TurnResult{}, session.ErrUserQueueUnavailable
	}
	defer e.releaseRecovery(key)
	input, frozen, err := recovery.PendingInterruptedUserQueue(ctx, sid)
	if err != nil {
		return agent.TurnResult{}, err
	}
	result, err := e.resumeInterruptedQueueTurn(ctx, input, frozen, store, resolve)
	if err != nil {
		err = errors.Join(err, recovery.PauseInterruptedUserQueue(context.WithoutCancel(ctx), sid))
	}
	return result, err
}

func (e *TurnExecutor) resumeInterruptedQueueTurn(ctx context.Context, input session.TurnInput, frozen session.QueueExecutionContext, store QueueExecutionStore, resolve QueueContextResolver) (agent.TurnResult, error) {
	current, err := resolve(ctx, input.SessionID, frozen)
	if err != nil {
		return agent.TurnResult{}, err
	}
	if current != frozen {
		return agent.TurnResult{}, session.ErrUserQueueContextChanged
	}
	release, err := e.beginSessionTurn(input.SessionID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer release()
	sess, err := e.repo.Load(ctx, input.SessionID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	if sess.TurnStatus() != session.TurnStatusPaused || !sessionHasOriginTurn(sess, input.TurnID) {
		return agent.TurnResult{}, session.ErrUserQueueUnavailable
	}
	queued := &queueExecution{frozen: frozen, store: store, resolve: resolve, resuming: true}
	return e.driveTurn(context.WithValue(ctx, queueExecutionKey{}, queued), sess, input.RequestID, input.Text, input.WireModel, frozen.ReasoningEffort, input.Assets, input.LocalAssets, &input, nil, nil, nil, session.TurnStatusResuming,
		func(runCtx context.Context, runner TurnRunner, _ bool) (agent.TurnResult, error) {
			return runner.ResumeTurn(runCtx, sess)
		}, e.recordRunStatus)
}
