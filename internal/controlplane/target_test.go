package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/managedworktree"
	"code-agent/internal/model"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/tools"
	"code-agent/internal/worktree"
)

type targetEventStore struct{ records []session.EventRecord }

type fakeManagedCreator struct {
	repo    conversation.ConversationRepository
	request managedworktree.CreateRequest
}

func (c *fakeManagedCreator) Create(ctx context.Context, request managedworktree.CreateRequest) (managedworktree.CreateResult, error) {
	c.request = request
	if existing, err := c.repo.Load(ctx, request.ReservedSessionID); err == nil {
		return managedworktree.CreateResult{Session: existing, Record: worktree.Record{SessionID: existing.ID, WorktreePath: existing.WorkspacePath, State: worktree.StateReady}}, nil
	}
	repo := c.repo.(conversation.ReservedConversationRepository)
	sess, err := repo.CreateWithID(ctx, request.ReservedSessionID, "/managed/"+request.ReservedSessionID, "")
	if err != nil {
		return managedworktree.CreateResult{}, err
	}
	sess.SetExecutionPolicy(session.ExecutionPolicyIsolatedWorktree)
	if err := c.repo.Save(ctx, sess); err != nil {
		return managedworktree.CreateResult{}, err
	}
	return managedworktree.CreateResult{Session: sess, Record: worktree.Record{SessionID: sess.ID, WorktreePath: sess.WorkspacePath, State: worktree.StateReady}}, nil
}

func (s *targetEventStore) Append(context.Context, session.EventRecord) (int64, error) { return 0, nil }
func (s *targetEventStore) Replay(context.Context, string) ([]session.EventRecord, error) {
	return append([]session.EventRecord(nil), s.records...), nil
}

func TestRuntimeTargetCreatesAndForksReservedSessionsIdempotently(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo := conversation.NewSQLiteRepository(store, 128000, 90000, "default-model", "", nil)
	target := NewRuntimeTarget(nil, nil, repo, nil)
	ctx := context.Background()
	workspace := t.TempDir()

	createRequest := tools.SessionCreateRequest{RequestID: "create-request", ParentSessionID: "parent", WorkspacePath: workspace, Name: "child", ExecutionPolicy: session.ExecutionPolicyReadOnly}
	created, err := target.CreateSession(ctx, createRequest, "created-child")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	createdAgain, err := target.CreateSession(ctx, createRequest, "created-child")
	if err != nil || createdAgain.ID != created.ID {
		t.Fatalf("idempotent CreateSession = %+v, %v", createdAgain, err)
	}
	createdSession, err := repo.Load(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if createdSession.Name != "child" || createdSession.ExecutionPolicy() != session.ExecutionPolicyReadOnly || createdSession.Metadata[session.MetaSpawnReady] != true {
		t.Fatalf("created session = %+v", createdSession)
	}

	source, err := repo.Create(ctx, workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	source.Name = "Source"
	source.Messages = append(source.Messages, model.Message{Role: model.RoleUser, Content: "durable input"})
	if err := repo.Save(ctx, source); err != nil {
		t.Fatal(err)
	}
	forkRequest := tools.SessionForkRequest{RequestID: "fork-request", ParentSessionID: "parent", SourceSessionID: source.ID}
	forked, err := target.ForkSession(ctx, forkRequest, "fork-child")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	forkedAgain, err := target.ForkSession(ctx, forkRequest, "fork-child")
	if err != nil || forkedAgain.ID != forked.ID {
		t.Fatalf("idempotent ForkSession = %+v, %v", forkedAgain, err)
	}
	forkedSession, err := repo.Load(ctx, forked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if forkedSession.Name != "Source (fork)" || len(forkedSession.Messages) != 2 || forkedSession.Messages[1].Content != "durable input" || forkedSession.Metadata[session.MetaSpawnReady] != true {
		t.Fatalf("forked session = %+v", forkedSession)
	}
}

func TestRuntimeTargetRejectsUnsupportedForkBeforeProvisioningChild(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo := conversation.NewSQLiteRepository(store, 128000, 90000, "default-model", "", nil)
	source, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	source.Messages = append(source.Messages, model.Message{Role: model.RoleUser, Assets: []model.GatewayAssetRef{{AssetID: 1}}})
	if err := repo.Save(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	target := NewRuntimeTarget(nil, nil, repo, nil)
	_, err = target.ForkSession(context.Background(), tools.SessionForkRequest{RequestID: "fork-assets", ParentSessionID: "parent", SourceSessionID: source.ID}, "must-not-exist")
	if !errors.Is(err, session.ErrForkAssetsUnsupported) {
		t.Fatalf("ForkSession error = %v", err)
	}
	if _, err := repo.Load(context.Background(), "must-not-exist"); err == nil {
		t.Fatal("unsupported fork provisioned an orphan child")
	}
}

func TestRuntimeTargetCreatesManagedWorktreeWithReservedChildID(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo := conversation.NewSQLiteRepository(store, 128000, 90000, "default-model", "", nil)
	managed := &fakeManagedCreator{repo: repo}
	target := NewRuntimeTarget(nil, nil, repo, managed)
	request := tools.SessionCreateRequest{
		RequestID: "managed-create", ParentSessionID: "parent", WorkspacePath: "/source/repo",
		Name: "Managed Child", ExecutionPolicy: session.ExecutionPolicyIsolatedWorktree,
		WorktreeName: "feature", BaseRef: string(worktree.BaseRefHead),
	}
	result, err := target.CreateSession(context.Background(), request, "managed-child")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "managed-child" || result.WorkspacePath != "/managed/managed-child" || managed.request.ReservedSessionID != "managed-child" || managed.request.SourceWorkspacePath != "/source/repo" || managed.request.SuggestedName != "feature" || managed.request.BaseRef != worktree.BaseRefHead {
		t.Fatalf("result=%+v request=%+v", result, managed.request)
	}
	loaded, err := repo.Load(context.Background(), result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "Managed Child" || loaded.ExecutionPolicy() != session.ExecutionPolicyIsolatedWorktree || loaded.Metadata[session.MetaSpawnReady] != true {
		t.Fatalf("managed child = %+v", loaded)
	}
}

func TestRuntimeTargetDoesNotDowngradeManagedCreate(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo := conversation.NewSQLiteRepository(store, 128000, 90000, "default-model", "", nil)
	target := NewRuntimeTarget(nil, nil, repo, nil)
	_, err = target.CreateSession(context.Background(), tools.SessionCreateRequest{
		RequestID: "managed-unavailable", ParentSessionID: "parent", WorkspacePath: "/source/repo",
		ExecutionPolicy: session.ExecutionPolicyIsolatedWorktree, BaseRef: string(worktree.BaseRefHead),
	}, "must-not-exist")
	if err == nil {
		t.Fatal("isolated create silently succeeded without managed-worktree capability")
	}
	if _, loadErr := repo.Load(context.Background(), "must-not-exist"); loadErr == nil {
		t.Fatal("isolated create silently provisioned a shared session")
	}
}
func (s *targetEventStore) ReplaySince(_ context.Context, _ string, cursor int64) ([]session.EventRecord, error) {
	var out []session.EventRecord
	for _, record := range s.records {
		if record.Seq > cursor {
			out = append(out, record)
		}
	}
	return out, nil
}

func TestRuntimeTargetWaitIsTurnAndCursorScoped(t *testing.T) {
	oldPayload, _ := json.Marshal(agent.Event{Kind: agent.EventTurnFinished, SessionID: "s", TurnID: "old"})
	newPayload, _ := json.Marshal(agent.Event{Kind: agent.EventTurnFinished, SessionID: "s", TurnID: "wanted"})
	store := &targetEventStore{records: []session.EventRecord{
		{Seq: 4, SessionID: "s", TurnID: "wanted", Kind: string(agent.EventTurnFinished), At: time.Now(), Payload: newPayload},
		{Seq: 6, SessionID: "s", TurnID: "old", Kind: string(agent.EventTurnFinished), At: time.Now(), Payload: oldPayload},
		{Seq: 9, SessionID: "s", TurnID: "wanted", Kind: string(agent.EventTurnFinished), At: time.Now(), Payload: newPayload},
	}}
	target := &RuntimeTarget{events: store}
	completion, latest, err := target.EventsSince(context.Background(), "s", "wanted", 5)
	if err != nil {
		t.Fatal(err)
	}
	if completion == nil || completion.Cursor != 9 || completion.TurnID != "wanted" || latest != 9 {
		t.Fatalf("completion=%+v latest=%d", completion, latest)
	}
	completion, latest, err = target.EventsSince(context.Background(), "s", "wanted", 9)
	if err != nil || completion != nil || latest != 9 {
		t.Fatalf("post-terminal completion=%+v latest=%d err=%v", completion, latest, err)
	}
}
