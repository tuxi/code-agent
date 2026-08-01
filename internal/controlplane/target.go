package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/managedworktree"
	"code-agent/internal/session"
	"code-agent/internal/sessionfork"
	"code-agent/internal/tools"
	"code-agent/internal/worktree"
)

type ManagedWorktreeCreator interface {
	Create(context.Context, managedworktree.CreateRequest) (managedworktree.CreateResult, error)
}

type RuntimeTarget struct {
	executor         *conversation.TurnExecutor
	events           conversation.ConversationEventStore
	repo             conversation.ConversationRepository
	managedWorktrees ManagedWorktreeCreator
}

func NewRuntimeTarget(executor *conversation.TurnExecutor, events conversation.ConversationEventStore, repo conversation.ConversationRepository, managedWorktrees ManagedWorktreeCreator) *RuntimeTarget {
	return &RuntimeTarget{executor: executor, events: events, repo: repo, managedWorktrees: managedWorktrees}
}

func (t *RuntimeTarget) CreateSession(ctx context.Context, request tools.SessionCreateRequest, childID string) (tools.SessionSpawnResult, error) {
	if request.ExecutionPolicy == session.ExecutionPolicyIsolatedWorktree {
		return t.createManagedWorktreeSession(ctx, request, childID)
	}
	repo, ok := t.repo.(conversation.ReservedConversationRepository)
	if !ok {
		return tools.SessionSpawnResult{}, errors.New("target repository does not support reserved session creation")
	}
	if request.ExecutionPolicy != session.ExecutionPolicySharedWorkspace && request.ExecutionPolicy != session.ExecutionPolicyReadOnly {
		return tools.SessionSpawnResult{}, fmt.Errorf("unsupported execution policy %q", request.ExecutionPolicy)
	}
	child, err := t.repo.Load(ctx, childID)
	if err == nil {
		existingRequest, _ := child.Metadata[session.MetaSpawnRequest].(string)
		if child.WorkspacePath != request.WorkspacePath || (existingRequest != "" && existingRequest != request.RequestID) || (existingRequest == "" && !child.IsEmpty()) {
			return tools.SessionSpawnResult{}, errors.New("reserved child session conflicts with an existing session")
		}
		if ready, _ := child.Metadata[session.MetaSpawnReady].(bool); ready {
			return spawnResult(child, request.ParentSessionID, "", session.SpawnKindCreate), nil
		}
	} else {
		child, err = repo.CreateWithID(ctx, childID, request.WorkspacePath, "")
		if err != nil {
			return tools.SessionSpawnResult{}, err
		}
	}
	child.SetExecutionPolicy(request.ExecutionPolicy)
	markSpawn(child, request.RequestID, request.ParentSessionID, "", session.SpawnKindCreate, true)
	child.Name = request.Name
	if err := t.repo.Save(ctx, child); err != nil {
		return tools.SessionSpawnResult{}, err
	}
	return spawnResult(child, request.ParentSessionID, "", session.SpawnKindCreate), nil
}

func (t *RuntimeTarget) createManagedWorktreeSession(ctx context.Context, request tools.SessionCreateRequest, childID string) (tools.SessionSpawnResult, error) {
	if t.managedWorktrees == nil {
		return tools.SessionSpawnResult{}, errors.New("managed worktree provisioning is not available on the target runtime")
	}
	suggestedName := request.WorktreeName
	if suggestedName == "" {
		suggestedName = request.Name
	}
	result, err := t.managedWorktrees.Create(ctx, managedworktree.CreateRequest{
		ClientRequestID: request.RequestID, ReservedSessionID: childID,
		SourceWorkspacePath: request.WorkspacePath, SuggestedName: suggestedName,
		BaseRef: worktree.BaseRef(request.BaseRef),
	})
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	if result.Session == nil || result.Session.ID != childID {
		return tools.SessionSpawnResult{}, errors.New("managed worktree provisioner returned a different child session")
	}
	result.Session.Name = request.Name
	markSpawn(result.Session, request.RequestID, request.ParentSessionID, "", session.SpawnKindCreate, true)
	if err := t.repo.Save(ctx, result.Session); err != nil {
		return tools.SessionSpawnResult{}, err
	}
	return spawnResult(result.Session, request.ParentSessionID, "", session.SpawnKindCreate), nil
}

func (t *RuntimeTarget) ForkSession(ctx context.Context, request sessionfork.Request, childID string) (sessionfork.Result, error) {
	if request.ExecutionPolicy == "" {
		request.ExecutionPolicy = session.ExecutionPolicySharedWorkspace
	}
	if request.ExecutionPolicy == session.ExecutionPolicyIsolatedWorktree {
		return t.forkManagedWorktreeSession(ctx, request, childID)
	}
	if request.ExecutionPolicy != session.ExecutionPolicySharedWorkspace {
		return sessionfork.Result{}, fmt.Errorf("unsupported fork execution policy %q", request.ExecutionPolicy)
	}
	repo, ok := t.repo.(conversation.ReservedConversationRepository)
	if !ok {
		return sessionfork.Result{}, errors.New("target repository does not support reserved session creation")
	}
	source, err := t.repo.Load(ctx, request.SourceSessionID)
	if err != nil {
		return sessionfork.Result{}, err
	}
	child, loadErr := t.repo.Load(ctx, childID)
	if loadErr == nil {
		existingRequest, _ := child.Metadata[session.MetaSpawnRequest].(string)
		existingSource, _ := child.Metadata[session.MetaForkSource].(string)
		if child.WorkspacePath != source.WorkspacePath || (existingRequest != "" && existingRequest != request.RequestID) || (existingSource != "" && existingSource != request.SourceSessionID) || (existingRequest == "" && !child.IsEmpty()) {
			return sessionfork.Result{}, errors.New("reserved fork session conflicts with an existing session")
		}
		if ready, _ := child.Metadata[session.MetaSpawnReady].(bool); ready {
			return forkResult(child, request.ParentSessionID, request.SourceSessionID, ""), nil
		}
	}
	if err := session.ValidateForkSource(source); err != nil {
		return sessionfork.Result{}, err
	}
	if loadErr != nil {
		child, err = repo.CreateWithID(ctx, childID, source.WorkspacePath, "")
		if err != nil {
			return sessionfork.Result{}, err
		}
		markSpawn(child, request.RequestID, request.ParentSessionID, request.SourceSessionID, session.SpawnKindFork, false)
		if err := t.repo.Save(ctx, child); err != nil {
			return sessionfork.Result{}, err
		}
	}
	if err := session.ForkHistoryInto(child, source, request.Name, time.Now().UTC()); err != nil {
		return sessionfork.Result{}, err
	}
	markSpawn(child, request.RequestID, request.ParentSessionID, request.SourceSessionID, session.SpawnKindFork, true)
	if err := t.repo.Save(ctx, child); err != nil {
		return sessionfork.Result{}, err
	}
	return forkResult(child, request.ParentSessionID, request.SourceSessionID, ""), nil
}

func (t *RuntimeTarget) forkManagedWorktreeSession(ctx context.Context, request sessionfork.Request, childID string) (sessionfork.Result, error) {
	if t.managedWorktrees == nil {
		return sessionfork.Result{}, errors.New("managed worktree provisioning is not available on the target runtime")
	}
	source, err := t.repo.Load(ctx, request.SourceSessionID)
	if err != nil {
		return sessionfork.Result{}, err
	}
	if err := session.ValidateForkHistory(source); err != nil {
		return sessionfork.Result{}, err
	}
	suggestedName := request.WorktreeName
	if suggestedName == "" {
		suggestedName = request.Name
	}
	baseWorkspaceID, _ := source.Metadata[session.MetaBaseWorkspaceID].(string)
	result, err := t.managedWorktrees.Create(ctx, managedworktree.CreateRequest{
		ClientRequestID: request.RequestID, ReservedSessionID: childID,
		SourceWorkspacePath: source.WorkspacePath, SourceWorkspaceID: source.ID,
		BaseWorkspaceID: baseWorkspaceID, SuggestedName: suggestedName,
		BaseRef: worktree.BaseRefHead, PinSourceHead: true, RequireClean: true,
		AllowLinkedSource: true,
	})
	if err != nil {
		return sessionfork.Result{}, err
	}
	if result.Session == nil || result.Session.ID != childID {
		return sessionfork.Result{}, errors.New("managed worktree provisioner returned a different fork session")
	}
	child := result.Session
	if ready, _ := child.Metadata[session.MetaSpawnReady].(bool); ready {
		return forkResult(child, request.ParentSessionID, request.SourceSessionID, result.Record.BaseCommit), nil
	}
	if err := session.ForkHistoryInto(child, source, request.Name, time.Now().UTC()); err != nil {
		return sessionfork.Result{}, err
	}
	markSpawn(child, request.RequestID, request.ParentSessionID, request.SourceSessionID, session.SpawnKindFork, true)
	if err := t.repo.Save(ctx, child); err != nil {
		return sessionfork.Result{}, err
	}
	return forkResult(child, request.ParentSessionID, request.SourceSessionID, result.Record.BaseCommit), nil
}

func markSpawn(child *session.Session, requestID, parentID, sourceID, kind string, ready bool) {
	if child.Metadata == nil {
		child.Metadata = make(map[string]any)
	}
	child.Metadata[session.MetaSpawnRequest] = requestID
	child.Metadata[session.MetaSpawnKind] = kind
	child.Metadata[session.MetaSpawnParent] = parentID
	child.Metadata[session.MetaSpawnReady] = ready
	if sourceID != "" {
		child.Metadata[session.MetaForkSource] = sourceID
	}
}

func spawnResult(child *session.Session, parentID, sourceID, kind string) tools.SessionSpawnResult {
	return tools.SessionSpawnResult{ID: child.ID, ParentSessionID: parentID, SourceSessionID: sourceID, WorkspacePath: child.WorkspacePath, Kind: kind, Status: "open"}
}

func forkResult(child *session.Session, parentID, sourceID, baseCommit string) sessionfork.Result {
	return sessionfork.Result{ID: child.ID, ParentSessionID: parentID, SourceSessionID: sourceID, WorkspacePath: child.WorkspacePath, Kind: session.SpawnKindFork, Status: "open", BaseCommit: baseCommit}
}

func (t *RuntimeTarget) Accept(ctx, executionCtx context.Context, sessionID string, request tools.SessionSendRequest) (tools.SessionDelivery, error) {
	admission, err := t.executor.AcceptCrossSessionMessage(ctx, executionCtx, sessionID, conversation.CrossSessionEnvelope{
		Text: request.Message, Model: request.Model, SenderSessionID: request.SenderSessionID,
		SenderTurnID: request.SenderTurnID, MessageID: request.MessageID,
		CorrelationID: request.CorrelationID, Intent: request.Intent,
	})
	if err != nil {
		return tools.SessionDelivery{}, err
	}
	return tools.SessionDelivery{Accepted: true, Delivery: admission.Delivery, SessionID: sessionID, TurnID: admission.TurnID, Cursor: admission.Cursor}, nil
}

func (t *RuntimeTarget) EventsSince(ctx context.Context, sessionID, turnID string, cursor int64) (*tools.SessionWaitCompletion, int64, error) {
	records, err := t.events.ReplaySince(ctx, sessionID, cursor)
	if err != nil {
		return nil, cursor, err
	}
	latest := cursor
	for _, record := range records {
		if record.Seq > latest {
			latest = record.Seq
		}
		if record.TurnID != turnID {
			continue
		}
		status := ""
		switch agent.EventKind(record.Kind) {
		case agent.EventTurnFinished:
			status = "completed"
		case agent.EventTurnFailed:
			status = "failed"
		case agent.EventTurnCancelled:
			status = "cancelled"
		}
		if status != "" {
			payload := append(json.RawMessage(nil), record.Payload...)
			return &tools.SessionWaitCompletion{SessionID: sessionID, TurnID: turnID, Status: status, Cursor: record.Seq, Event: payload}, latest, nil
		}
	}
	return nil, latest, nil
}
