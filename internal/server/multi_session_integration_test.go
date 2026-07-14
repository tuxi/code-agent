package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/runtime"
	"code-agent/internal/session"

	"github.com/coder/websocket"
)

type approvalAttentionBuilder struct{}

func (approvalAttentionBuilder) Build(rc conversation.RuntimeContext) conversation.TurnRunner {
	return &approvalAttentionRunner{rc: rc}
}

type approvalAttentionRunner struct {
	rc conversation.RuntimeContext
}

func (r *approvalAttentionRunner) RunTurn(_ context.Context, sess *session.Session, _ string) (agent.TurnResult, error) {
	r.rc.Publisher.Emit(agent.Event{Kind: agent.EventTurnStarted, SessionID: sess.ID, TurnID: r.rc.TurnID})
	if r.rc.Approver.Approve("run_command", json.RawMessage(`{"command":"true"}`)) != agent.VerdictAllow {
		return agent.TurnResult{TurnID: r.rc.TurnID}, context.Canceled
	}
	r.rc.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: sess.ID, TurnID: r.rc.TurnID, Text: "done"})
	return agent.TurnResult{TurnID: r.rc.TurnID, Final: "done"}, nil
}

func (r *approvalAttentionRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	return r.RunTurn(ctx, sess, "")
}

type multiSessionRunBuilder struct {
	started chan string
	mu      sync.Mutex
	release map[string]chan struct{}
}

func newMultiSessionRunBuilder() *multiSessionRunBuilder {
	return &multiSessionRunBuilder{started: make(chan string, 8), release: make(map[string]chan struct{})}
}

func (b *multiSessionRunBuilder) Build(rc conversation.RuntimeContext) conversation.TurnRunner {
	b.mu.Lock()
	release := b.release[rc.Session.ID]
	if release == nil {
		release = make(chan struct{})
		b.release[rc.Session.ID] = release
	}
	b.mu.Unlock()
	return &multiSessionRunner{rc: rc, started: b.started, release: release}
}

func (b *multiSessionRunBuilder) finish(sessionID string) {
	b.mu.Lock()
	ch := b.release[sessionID]
	delete(b.release, sessionID)
	b.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

type multiSessionRunner struct {
	rc      conversation.RuntimeContext
	started chan<- string
	release <-chan struct{}
}

func (r *multiSessionRunner) RunTurn(ctx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	r.rc.Publisher.Emit(agent.Event{Kind: agent.EventTurnStarted, SessionID: sess.ID, TurnID: r.rc.TurnID, Text: input})
	r.started <- sess.ID
	select {
	case <-r.release:
		r.rc.Publisher.Emit(agent.Event{Kind: agent.EventTurnFinished, SessionID: sess.ID, TurnID: r.rc.TurnID, Text: "done"})
		return agent.TurnResult{TurnID: r.rc.TurnID, Final: "done"}, nil
	case <-ctx.Done():
		return agent.TurnResult{TurnID: r.rc.TurnID}, ctx.Err()
	}
}

func (r *multiSessionRunner) ResumeTurn(ctx context.Context, sess *session.Session) (agent.TurnResult, error) {
	return r.RunTurn(ctx, sess, "")
}

func TestMultiSessionWebSocketRunsDifferentWorkspacesConcurrently(t *testing.T) {
	repo := newFakeConversationRepo()
	repo.sessions["a"] = &session.Session{ID: "a", WorkspacePath: "/workspace/a", Metadata: map[string]any{}}
	repo.sessions["b"] = &session.Session{ID: "b", WorkspacePath: "/workspace/b", Metadata: map[string]any{}}
	events := &fakeEventStore{}
	builder := newMultiSessionRunBuilder()
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), builder)
	executor.SetTurnScheduler(conversation.NewTurnScheduler(2))
	srv := httptest.NewServer(NewMux(repo, events, executor, MuxOptions{Accept: &websocket.AcceptOptions{InsecureSkipVerify: true}}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := dialConversation(t, ctx, srv.URL, "a")
	defer a.CloseNow()
	b := dialConversation(t, ctx, srv.URL, "b")
	defer b.CloseNow()
	wsWriteJSON(t, ctx, a, map[string]any{"type": "agent_input", "kind": "text", "request_id": "req-a", "text": "A"})
	wsWriteJSON(t, ctx, b, map[string]any{"type": "agent_input", "kind": "text", "request_id": "req-b", "text": "B"})

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-builder.started:
			seen[id] = true
		case <-ctx.Done():
			t.Fatal("different workspaces did not start concurrently")
		}
	}
	builder.finish("a")
	builder.finish("b")
}

func TestManagedWorktreeHTTPAndWebSocketRunSameRepositoryConcurrently(t *testing.T) {
	root := initManagedHTTPRepo(t)
	store := session.NewMemoryStore()
	repo := newFakeConversationRepo()
	events := &fakeEventStore{}
	builder := newMultiSessionRunBuilder()
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), builder)
	executor.SetTurnScheduler(conversation.NewTurnScheduler(2))
	manager, _, err := runtime.ConfigureManagedWorktrees(context.Background(), store, repo, executor, true)
	if err != nil || manager == nil {
		t.Fatalf("manager=%v err=%v", manager, err)
	}
	caps := ConfiguredRuntimeCapabilities(2)
	caps.ManagedWorktree = true
	srv := httptest.NewServer(NewMux(repo, events, executor, MuxOptions{
		Accept: &websocket.AcceptOptions{InsecureSkipVerify: true}, RuntimeCapabilities: caps, ManagedWorktrees: manager,
	}))
	defer srv.Close()

	create := func(requestID, name string) ConversationRef {
		response := requestJSON(t, srv.URL+"/v1/conversations", map[string]any{
			"client_request_id": requestID, "workspace_path": root,
			"base_workspace_id": "same_repo", "execution_policy": session.ExecutionPolicyIsolatedWorktree,
			"worktree": map[string]any{"managed": true, "suggested_name": name, "base_ref": "head"},
		})
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", response.StatusCode, readManagedHTTPBody(t, response))
		}
		var ref ConversationRef
		decodeResponse(t, response, &ref)
		return ref
	}
	aRef := create("create_ws_a", "ws-a")
	bRef := create("create_ws_b", "ws-b")
	if aRef.WorkspacePath == bRef.WorkspacePath {
		t.Fatal("managed creates returned the same checkout")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	a := dialConversation(t, ctx, srv.URL, aRef.ID)
	defer a.CloseNow()
	b := dialConversation(t, ctx, srv.URL, bRef.ID)
	defer b.CloseNow()
	wsWriteJSON(t, ctx, a, map[string]any{"type": "agent_input", "kind": "text", "request_id": "turn-a", "text": "A"})
	wsWriteJSON(t, ctx, b, map[string]any{"type": "agent_input", "kind": "text", "request_id": "turn-b", "text": "B"})
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-builder.started:
			seen[id] = true
		case <-ctx.Done():
			t.Fatal("two managed worktrees from one repository did not run concurrently")
		}
	}
	builder.finish(aRef.ID)
	builder.finish(bRef.ID)
}

func TestMultiSessionWebSocketQueuesAndCancelsSharedWorkspace(t *testing.T) {
	repo := newFakeConversationRepo()
	repo.sessions["a"] = &session.Session{ID: "a", WorkspacePath: "/workspace/shared", Metadata: map[string]any{}}
	repo.sessions["b"] = &session.Session{ID: "b", WorkspacePath: "/workspace/shared", Metadata: map[string]any{}}
	events := &fakeEventStore{}
	builder := newMultiSessionRunBuilder()
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), builder)
	executor.SetTurnScheduler(conversation.NewTurnScheduler(2))
	srv := httptest.NewServer(NewMux(repo, events, executor, MuxOptions{Accept: &websocket.AcceptOptions{InsecureSkipVerify: true}}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := dialConversation(t, ctx, srv.URL, "a")
	defer a.CloseNow()
	b := dialConversation(t, ctx, srv.URL, "b")
	defer b.CloseNow()
	wsWriteJSON(t, ctx, a, map[string]any{"type": "agent_input", "kind": "text", "request_id": "req-a", "text": "A"})
	select {
	case id := <-builder.started:
		if id != "a" {
			t.Fatalf("started=%q", id)
		}
	case <-ctx.Done():
		t.Fatal("first shared-workspace turn did not start")
	}

	wsWriteJSON(t, ctx, b, map[string]any{"type": "agent_input", "kind": "text", "request_id": "req-b", "text": "B"})
	queued := readUntilKind(t, ctx, b, string(agent.EventTurnQueued))
	if queued["turn_id"] == "" || queued["queue_position"].(float64) != 1 || queued["reason"] != string(conversation.QueueReasonWorkspaceLease) {
		t.Fatalf("queued frame=%v", queued)
	}
	activity := fetchActivity(t, srv.URL)
	var queuedActivity *SessionActivity
	for i := range activity.Sessions {
		if activity.Sessions[i].SessionID == "b" {
			queuedActivity = &activity.Sessions[i]
			break
		}
	}
	if queuedActivity == nil || queuedActivity.State != "queued" || queuedActivity.QueueReason != string(conversation.QueueReasonWorkspaceLease) {
		t.Fatalf("queued activity=%+v", queuedActivity)
	}
	wsWriteJSON(t, ctx, b, map[string]any{"type": "cancel_turn"})
	cancelled := readUntilKind(t, ctx, b, string(agent.EventTurnCancelled))
	if cancelled["turn_id"] != queued["turn_id"] {
		t.Fatalf("queued/cancelled ids differ: %v / %v", queued, cancelled)
	}

	events.mu.Lock()
	cancelledCount := 0
	for _, record := range events.recs["b"] {
		if record.Kind == string(agent.EventTurnCancelled) {
			cancelledCount++
		}
	}
	events.mu.Unlock()
	if cancelledCount != 1 {
		t.Fatalf("turn_cancelled count=%d", cancelledCount)
	}
	builder.finish("a")
}

func TestArchivedConversationRejectsWebSocketTurnUntilRestored(t *testing.T) {
	store := session.NewMemoryStore()
	repo := conversation.NewSQLiteRepository(store, 128000, 90000, "test", "", nil)
	sess, err := repo.Create(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	events := &fakeEventStore{}
	builder := newMultiSessionRunBuilder()
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), builder)
	srv := httptest.NewServer(NewMux(repo, events, executor, MuxOptions{
		Accept: &websocket.AcceptOptions{InsecureSkipVerify: true}, RuntimeCapabilities: ConfiguredRuntimeCapabilities(2),
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialConversation(t, ctx, srv.URL, sess.ID)
	defer conn.CloseNow()
	archived, err := http.Post(srv.URL+"/v1/conversations/"+sess.ID+"/archive", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	archived.Body.Close()
	if archived.StatusCode != http.StatusOK {
		t.Fatalf("archive status=%d", archived.StatusCode)
	}
	wsWriteJSON(t, ctx, conn, map[string]any{"type": "agent_input", "kind": "text", "request_id": "archived-turn", "text": "must not run"})
	select {
	case id := <-builder.started:
		t.Fatalf("archived conversation started turn for %q", id)
	case <-time.After(50 * time.Millisecond):
	}

	restored, err := http.Post(srv.URL+"/v1/conversations/"+sess.ID+"/restore", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	restored.Body.Close()
	if restored.StatusCode != http.StatusOK {
		t.Fatalf("restore status=%d", restored.StatusCode)
	}
	wsWriteJSON(t, ctx, conn, map[string]any{"type": "agent_input", "kind": "text", "request_id": "restored-turn", "text": "run"})
	select {
	case id := <-builder.started:
		if id != sess.ID {
			t.Fatalf("restored turn started session=%q", id)
		}
	case <-ctx.Done():
		t.Fatal("restored conversation did not start")
	}
	builder.finish(sess.ID)
}

func TestMultiSessionActivityReportsBackgroundApprovalAndTerminal(t *testing.T) {
	repo := newFakeConversationRepo()
	repo.sessions["background"] = &session.Session{ID: "background", WorkspacePath: "/workspace/background", Metadata: map[string]any{}}
	events := &fakeEventStore{}
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), approvalAttentionBuilder{})
	executor.SetTurnScheduler(conversation.NewTurnScheduler(2))
	srv := httptest.NewServer(NewMux(repo, events, executor, MuxOptions{
		Accept:              &websocket.AcceptOptions{InsecureSkipVerify: true},
		RuntimeCapabilities: ConfiguredRuntimeCapabilities(2),
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialConversation(t, ctx, srv.URL, "background")
	defer conn.CloseNow()
	wsWriteJSON(t, ctx, conn, map[string]any{"type": "agent_input", "kind": "text", "request_id": "req-attention", "text": "approve"})

	var approvalID string
	for approvalID == "" {
		frame := readFrame(t, ctx, conn)
		if frame["type"] == "approval_request" {
			approvalID, _ = frame["id"].(string)
		}
	}
	activity := fetchActivity(t, srv.URL)
	if len(activity.Sessions) != 1 || activity.Sessions[0].State != "waiting_approval" || activity.Sessions[0].PendingApprovalCount != 1 || activity.Sessions[0].ActiveTurnID == "" {
		t.Fatalf("pending approval activity=%+v", activity.Sessions)
	}
	activeTurnID := activity.Sessions[0].ActiveTurnID

	wsWriteJSON(t, ctx, conn, map[string]any{"type": "approval_response", "id": approvalID, "approved": true})
	readUntilKind(t, ctx, conn, string(agent.EventTurnFinished))
	deadline := time.Now().Add(time.Second)
	for len(executor.Activity()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("turn did not leave scheduler after terminal")
		}
		time.Sleep(time.Millisecond)
	}
	activity = fetchActivity(t, srv.URL)
	if len(activity.Sessions) != 1 || activity.Sessions[0].PendingApprovalCount != 0 || activity.Sessions[0].State != "idle" || activity.Sessions[0].LatestTerminal == nil || activity.Sessions[0].LatestTerminal.TurnID != activeTurnID || activity.Sessions[0].LatestTerminal.Kind != string(agent.EventTurnFinished) || activity.Sessions[0].LatestTerminal.Sequence == 0 {
		t.Fatalf("terminal activity=%+v", activity.Sessions)
	}
}

func fetchActivity(t *testing.T, serverURL string) activityResponse {
	t.Helper()
	resp, err := http.Get(serverURL + "/v1/activity")
	if err != nil {
		t.Fatal(err)
	}
	var activity activityResponse
	decodeResponse(t, resp, &activity)
	return activity
}

func dialConversation(t *testing.T, ctx context.Context, serverURL, sessionID string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/conversations/" + sessionID + "/stream"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	readType(t, ctx, conn, "hello")
	return conn
}

func readUntilKind(t *testing.T, ctx context.Context, conn *websocket.Conn, kind string) map[string]any {
	t.Helper()
	for {
		frame := readFrame(t, ctx, conn)
		if frame["kind"] == kind {
			return frame
		}
	}
}
