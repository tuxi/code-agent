package conversation

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"code-agent/internal/collab"
	"code-agent/internal/session"
)

type managedAdmissionKey struct{}

func managedAdmissionRef(ctx context.Context) *collab.ManagedTurnRef {
	ref, ok := ctx.Value(managedAdmissionKey{}).(collab.ManagedTurnRef)
	if !ok {
		return nil
	}
	return &ref
}

// AcceptManagedDispatch is a host-only port: callers select a persisted
// reservation, not a prompt, approval mode, or internal model parameters.
// Duplicate delivery only observes the original Turn; it never resumes it.
func (e *TurnExecutor) AcceptManagedDispatch(ctx, executionCtx context.Context, a collab.Principal, ref collab.ManagedTurnRef) (TurnAdmission, error) {
	provider, ok := e.repo.(collab.RepositoryProvider)
	if !ok {
		return TurnAdmission{}, collab.ErrUnavailable
	}
	repo, ok := provider.TaskRepository(ref.ServerID).(collab.ManagedRepository)
	if !ok {
		return TurnAdmission{}, collab.ErrUnavailable
	}
	if _, ok = e.repo.(TurnInputRepository); !ok {
		return TurnAdmission{}, collab.ErrUnavailable
	}
	if _, ok = e.repo.(collab.ExecutionGuard); !ok {
		return TurnAdmission{}, collab.ErrUnavailable
	}
	// Serialize only the acceptance handshake. Target execution continues after
	// this lock is released, so a duplicate can return while approvals are pending.
	e.requestMu.Lock()
	defer e.requestMu.Unlock()
	d, err := repo.Dispatch(ctx, a, ref)
	if err != nil {
		return TurnAdmission{}, err
	}
	if stored, err := e.repo.(TurnInputRepository).TurnInput(ctx, d.SessionID, d.RequestID); err == nil {
		if stored.ManagedTask == nil || *stored.ManagedTask != ref || stored.Text != d.Message || (d.Model != "" && stored.WireModel != d.Model) {
			return TurnAdmission{}, collab.ErrConflict
		}
		return TurnAdmission{Delivery: admissionDelivery(stored.State), TurnID: stored.TurnID, Cursor: stored.AcceptedSeq}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return TurnAdmission{}, err
	}
	if executionCtx == nil {
		executionCtx = context.Background()
	}
	executionCtx = context.WithValue(executionCtx, managedAdmissionKey{}, ref)
	results := make(chan admissionResult, 1)
	var once sync.Once
	notify := func(admission TurnAdmission, err error) {
		once.Do(func() { results <- admissionResult{admission: admission, err: err} })
	}
	go func() {
		release, err := e.beginSessionTurn(d.SessionID)
		if err != nil {
			notify(TurnAdmission{}, err)
			return
		}
		defer release()
		sess, err := e.repo.Load(executionCtx, d.SessionID)
		if err == nil && !sess.ArchivedAt.IsZero() {
			err = ErrConversationArchived
		}
		if err == nil {
			model := d.Model
			if model == "" {
				model = sess.Model
			}
			_, err = e.executeWithSessionAssets(executionCtx, sess, d.RequestID, d.Message, model, "", nil, nil, nil, notify)
		}
		notify(TurnAdmission{}, err)
	}()
	select {
	case result := <-results:
		return result.admission, result.err
	case <-ctx.Done():
		return TurnAdmission{}, ctx.Err()
	}
}

func (e *TurnExecutor) acquireTaskExecution(ctx context.Context, sess *session.Session, requestID string, preclaimed *session.TurnInput) (func(), error) {
	ref := managedAdmissionRef(ctx)
	guard, ok := e.repo.(collab.ExecutionGuard)
	if preclaimed != nil && preclaimed.ManagedTask != nil {
		if ok {
			if err := guard.RequireTaskRecovery(ctx, sess.ID, preclaimed.RequestID); err != nil {
				return nil, err
			}
		}
		return nil, collab.ErrRecoveryRequired
	}
	if !ok {
		if ref != nil {
			return nil, collab.ErrUnavailable
		}
		return func() {}, nil
	}
	return guard.AcquireTaskExecution(ctx, sess.ID, sess.WorkspacePath, requestID, ref)
}
