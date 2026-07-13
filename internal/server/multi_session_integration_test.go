package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"

	"github.com/coder/websocket"
)

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
	if queued["turn_id"] == "" || queued["queue_position"].(float64) != 1 {
		t.Fatalf("queued frame=%v", queued)
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
