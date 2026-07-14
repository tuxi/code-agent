package runtime

import (
	"context"

	"code-agent/internal/conversation"
	"code-agent/internal/managedworktree"
	"code-agent/internal/session"
	"code-agent/internal/worktree"
)

type managedConversationRepository interface {
	conversation.ConversationRepository
	conversation.ReservedConversationRepository
}

// ConfigureManagedWorktrees installs the optional desktop Git provisioner only
// when every persistence/creation extension is actually present. Capability
// wiring uses a non-nil return, so custom stores safely remain disabled.
func ConfigureManagedWorktrees(
	ctx context.Context,
	store session.Store,
	repo conversation.ConversationRepository,
	executor *conversation.TurnExecutor,
	allowGit bool,
) (*managedworktree.Manager, managedworktree.ReconcileReport, error) {
	if !allowGit || executor == nil {
		return nil, managedworktree.ReconcileReport{}, nil
	}
	worktreeStore, storeOK := any(store).(worktree.Store)
	managedRepo, repoOK := repo.(managedConversationRepository)
	if !storeOK || !repoOK {
		return nil, managedworktree.ReconcileReport{}, nil
	}
	manager := managedworktree.New(worktreeStore, managedRepo)
	report, err := manager.Reconcile(ctx)
	if err != nil {
		return nil, report, err
	}
	manager.SetBusyChecker(executor.HasActivity)
	executor.SetExecutionGuard(manager.AcquireTurnGuard)
	return manager, report, nil
}
