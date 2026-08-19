package controlplane

import (
	"code-agent/internal/settings"
	"context"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

type workflowRun struct {
	SessionID     string
	WorkspacePath string
	TurnID        string
	Input         string
}

type workflowRunBuilder struct {
	mu   sync.Mutex
	runs []workflowRun
}

func (b *workflowRunBuilder) ResolveModel(string) (*settings.ModelConfig, error) {
	m := settings.ModelConfig{Model: "workflow-test-model"}
	return &m, nil
}

func (b *workflowRunBuilder) Build(rc conversation.RuntimeContext) conversation.TurnRunner {
	run := workflowRun{SessionID: rc.Session.ID, WorkspacePath: rc.Session.WorkspacePath, TurnID: rc.TurnID}
	for i := len(rc.Session.Messages) - 1; i >= 0; i-- {
		if rc.Session.Messages[i].Role == model.RoleUser {
			run.Input = rc.Session.Messages[i].Content
			break
		}
	}
	b.mu.Lock()
	b.runs = append(b.runs, run)
	b.mu.Unlock()
	return workflowRunner{publisher: rc.Publisher, sessionID: rc.Session.ID, turnID: rc.TurnID}
}

func (b *workflowRunBuilder) snapshot() []workflowRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]workflowRun(nil), b.runs...)
}

type workflowRunner struct {
	publisher agent.Emitter
	sessionID string
	turnID    string
}

func (r workflowRunner) RunTurn(context.Context, *session.Session, string) (agent.TurnResult, error) {
	return r.finish(), nil
}

func (r workflowRunner) ResumeTurn(context.Context, *session.Session) (agent.TurnResult, error) {
	return r.finish(), nil
}

func (r workflowRunner) finish() agent.TurnResult {
	r.publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: r.sessionID, TurnID: r.turnID, Text: "completed"})
	return agent.TurnResult{TurnID: r.turnID, Final: "completed"}
}

func TestCrossWorkspaceFrontendBackendWorkflowReactivatesCompletedFrontend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	controlRoot := t.TempDir()
	frontendWorkspace := t.TempDir()
	backendWorkspace := t.TempDir()

	frontendStore := session.NewMemoryStore()
	frontendRepo := conversation.NewSQLiteRepository(frontendStore, 128000, 90000, "workflow-test-model", "", nil)
	frontendReserved := frontendRepo.(conversation.ReservedConversationRepository)
	for _, item := range []struct {
		id        string
		workspace string
	}{{"coordinator", frontendWorkspace}, {"frontend", frontendWorkspace}} {
		if _, err := frontendReserved.CreateWithID(ctx, item.id, item.workspace, ""); err != nil {
			t.Fatal(err)
		}
	}
	frontendEvents := &conversation.StoreEventAdapter{Store: frontendStore}
	frontendBuilder := &workflowRunBuilder{}
	frontendExecutor := conversation.NewTurnExecutor(frontendRepo, frontendEvents, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), frontendBuilder)
	frontendOwner, err := NewManager(controlRoot, "frontend-runtime", frontendRepo, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	frontendOwner.SetTarget(NewRuntimeTarget(frontendExecutor, frontendEvents, frontendRepo, nil))

	backendStore := session.NewMemoryStore()
	backendRepo := conversation.NewSQLiteRepository(backendStore, 128000, 90000, "workflow-test-model", "", nil)
	backendReserved := backendRepo.(conversation.ReservedConversationRepository)
	if _, err := backendReserved.CreateWithID(ctx, "backend", backendWorkspace, ""); err != nil {
		t.Fatal(err)
	}
	backendEvents := &conversation.StoreEventAdapter{Store: backendStore}
	backendBuilder := &workflowRunBuilder{}
	backendExecutor := conversation.NewTurnExecutor(backendRepo, backendEvents, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), backendBuilder)
	backendOwner, err := NewManager(controlRoot, "backend-runtime", backendRepo, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	backendOwner.SetTarget(NewRuntimeTarget(backendExecutor, backendEvents, backendRepo, nil))

	if err := frontendOwner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := backendOwner.Start(ctx); err != nil {
		_ = frontendOwner.Close()
		t.Fatal(err)
	}
	localOwners.Lock()
	delete(localOwners.byInstance, frontendOwner.Identity().InstanceID)
	delete(localOwners.byInstance, backendOwner.Identity().InstanceID)
	localOwners.Unlock()
	t.Cleanup(func() {
		localOwners.Lock()
		localOwners.byInstance[frontendOwner.Identity().InstanceID] = frontendOwner
		localOwners.byInstance[backendOwner.Identity().InstanceID] = backendOwner
		localOwners.Unlock()
		_ = frontendOwner.Close()
		_ = backendOwner.Close()
	})

	frontendInitial := sendWorkflowMessage(t, ctx, frontendOwner, tools.SessionSendRequest{
		TargetSessionID: "frontend", Message: "Build the initial Todo UI and API adapter.",
		SenderSessionID: "coordinator", SenderTurnID: "plan-turn", MessageID: "frontend-initial",
		CorrelationID: "todo-case/frontend/initial", Intent: "request",
	})
	backendInitial := sendWorkflowMessage(t, ctx, frontendOwner, tools.SessionSendRequest{
		TargetSessionID: "backend", Message: "Build the Todo HTTP API and publish its contract.",
		SenderSessionID: "coordinator", SenderTurnID: "plan-turn", MessageID: "backend-initial",
		CorrelationID: "todo-case/backend/initial", Intent: "request",
	})
	waitWorkflowTurn(t, ctx, frontendOwner, frontendInitial)
	waitWorkflowTurn(t, ctx, frontendOwner, backendInitial)

	contract := sendWorkflowMessage(t, ctx, backendOwner, tools.SessionSendRequest{
		TargetSessionID: "frontend", Message: "Integrate GET/POST/PATCH /api/tasks at http://127.0.0.1:18080.",
		SenderSessionID: "backend", SenderTurnID: backendInitial.TurnID, MessageID: "backend-contract",
		CorrelationID: "todo-case/backend/frontend-contract", Intent: "request",
	})
	if contract.TurnID == frontendInitial.TurnID || contract.Cursor <= frontendInitial.Cursor {
		t.Fatalf("frontend was not admitted as a distinct later turn: initial=%+v contract=%+v", frontendInitial, contract)
	}
	waitWorkflowTurn(t, ctx, backendOwner, contract)

	frontendRuns := frontendBuilder.snapshot()
	if len(frontendRuns) != 2 || frontendRuns[0].Input != "Build the initial Todo UI and API adapter." || frontendRuns[1].Input != "Integrate GET/POST/PATCH /api/tasks at http://127.0.0.1:18080." {
		t.Fatalf("frontend runs=%+v", frontendRuns)
	}
	if frontendRuns[0].WorkspacePath != frontendWorkspace || frontendRuns[1].WorkspacePath != frontendWorkspace {
		t.Fatalf("frontend escaped workspace: %+v", frontendRuns)
	}
	backendRuns := backendBuilder.snapshot()
	if len(backendRuns) != 1 || backendRuns[0].WorkspacePath != backendWorkspace || backendRuns[0].Input != "Build the Todo HTTP API and publish its contract." {
		t.Fatalf("backend runs=%+v", backendRuns)
	}
	storedContract, err := frontendStore.TurnInput(ctx, "frontend", "backend-contract")
	if err != nil {
		t.Fatal(err)
	}
	if storedContract.State != session.TurnInputCompleted || storedContract.Provenance == nil || storedContract.Provenance.SenderSessionID != "backend" || storedContract.Provenance.CorrelationID != "todo-case/backend/frontend-contract" {
		t.Fatalf("contract provenance=%+v state=%q", storedContract.Provenance, storedContract.State)
	}
}

func sendWorkflowMessage(t *testing.T, ctx context.Context, router *Manager, request tools.SessionSendRequest) tools.SessionDelivery {
	t.Helper()
	delivery, err := router.Send(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !delivery.Accepted || delivery.TurnID == "" || delivery.Cursor <= 0 {
		t.Fatalf("invalid delivery: %+v", delivery)
	}
	return delivery
}

func waitWorkflowTurn(t *testing.T, ctx context.Context, router *Manager, delivery tools.SessionDelivery) {
	t.Helper()
	result, err := router.WaitAny(ctx, []tools.SessionWaitTarget{{
		SessionID: delivery.SessionID, TurnID: delivery.TurnID, Cursor: delivery.Cursor,
	}}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Completed) != 1 || result.Completed[0].Status != "completed" || result.Completed[0].TurnID != delivery.TurnID {
		t.Fatalf("wait result=%+v", result)
	}
}
