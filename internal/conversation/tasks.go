package conversation

import (
	"code-agent/internal/collab"
	"context"
)

// Optional passthrough, matching the archive/inbox extensions. Unsupported
// stores return nil and do not publish Task endpoints or capabilities.
func (r *sqliteRepository) TaskRepository(serverID string) collab.Repository {
	if port, ok := r.store.(collab.RepositoryProvider); ok {
		return port.TaskRepository(serverID)
	}
	return nil
}

func (r *sqliteRepository) AcquireTaskExecution(ctx context.Context, sessionID, path, requestID string, ref *collab.ManagedTurnRef) (func(), error) {
	if guard, ok := r.store.(collab.ExecutionGuard); ok {
		return guard.AcquireTaskExecution(ctx, sessionID, path, requestID, ref)
	}
	if ref != nil {
		return nil, collab.ErrUnavailable
	}
	return func() {}, nil
}

func (r *sqliteRepository) RequireTaskRecovery(ctx context.Context, sessionID, requestID string) error {
	if guard, ok := r.store.(collab.ExecutionGuard); ok {
		return guard.RequireTaskRecovery(ctx, sessionID, requestID)
	}
	return collab.ErrUnavailable
}
