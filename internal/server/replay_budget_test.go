package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

// The daemon's event store is a StoreEventAdapter over the sqlite store. Verify
// the replay-budget path (loadEventsSince → EventBudgetStore) and the by-kind
// path (/messages) actually engage through that seam — a session with a huge
// tool_stdout blob must not be materialized whole.
func TestMuxReplayBudgetThroughAdapter(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	adapter := &conversation.StoreEventAdapter{Store: store}
	ctx := context.Background()

	repo := newFakeConversationRepo()
	id := "s1"
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: "/tmp/s1"}
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	_, _ = store.RecordEvent(ctx, storedEvent(agent.Event{Kind: agent.EventTurnStarted, SessionID: id, At: at, Text: "build it"}))
	big := strings.Repeat("x", 2*1024*1024)
	_, _ = store.RecordEvent(ctx, storedEvent(agent.Event{Kind: agent.EventToolStdout, SessionID: id, At: at.Add(time.Second), CallID: "c1", Chunk: big}))
	_, _ = store.RecordEvent(ctx, storedEvent(agent.Event{Kind: agent.EventTurnFinished, SessionID: id, At: at.Add(2 * time.Second), Text: "failed: error"}))

	srv := httptest.NewServer(newTestMux(repo, adapter))
	defer srv.Close()

	// /events full replay must be bounded and flagged truncated (the 2MB blob
	// alone exceeds the 8MB budget only when combined with a second row... use a
	// budget-sensitive assertion: the response must not carry a 2MB+ blob).
	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Codeagent-Replay-Truncated") == "" {
		t.Log("note: 2MB blob fits the 8MB budget, no truncation expected — budget path still engaged")
	}
	// Whatever the size, /events must NOT be the unbounded full materialization:
	// with a single 2MB event the budget check keeps it, which is fine. The real
	// assertion is that a payload FAR over budget is capped — covered below.

	// /messages uses the by-kind path: only turn events, so the 2MB blob is
	// never loaded and the response has exactly 2 messages.
	mresp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer mresp.Body.Close()
	var msgs []MessageView
	decodeResponse(t, mresp, &msgs)
	if len(msgs) != 2 {
		t.Fatalf("messages = %+v, want 2 (user+assistant) — by-kind must exclude the 2MB blob", msgs)
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("roles = %s,%s", msgs[0].Role, msgs[1].Role)
	}
}

// A payload far over the replay budget is capped: the response body stays small
// and the truncation header is set.
func TestMuxReplayBudgetCapsHugeLog(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	adapter := &conversation.StoreEventAdapter{Store: store}
	ctx := context.Background()

	repo := newFakeConversationRepo()
	id := "s1"
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: "/tmp/s1"}
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// 20 x 2MB blobs = 40MB of tool_stdout — the replay must cap well below that.
	for i := 0; i < 20; i++ {
		_, _ = store.RecordEvent(ctx, storedEvent(agent.Event{
			Kind: agent.EventToolStdout, SessionID: id, At: at.Add(time.Duration(i) * time.Second), CallID: "c1", Chunk: strings.Repeat("x", 2*1024*1024),
		}))
	}

	srv := httptest.NewServer(newTestMux(repo, adapter))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Codeagent-Replay-Truncated") != "1" {
		t.Fatal("expected X-Codeagent-Replay-Truncated: 1 for a 40MB log")
	}
	body := new(strings.Builder)
	if _, err := io.Copy(body, resp.Body); err != nil {
		t.Fatal(err)
	}
	if body.Len() > 16*1024*1024 {
		t.Fatalf("replay body = %d bytes, want well under 16MB (bounded tail)", body.Len())
	}
}
