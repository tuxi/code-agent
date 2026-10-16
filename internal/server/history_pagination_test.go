package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

type pageSnapshot struct {
	Oldest    string   `json:"oldest,omitempty"`
	HasMore   string   `json:"has_more"`
	Truncated string   `json:"truncated,omitempty"`
	Fragment  string   `json:"fragment,omitempty"`
	Seqs      []int64  `json:"seqs"`
	Kinds     []string `json:"kinds"`
}

type eventStoreWithoutPage struct{ session.EventStore }

func readPage(t *testing.T, url string) (int, pageSnapshot) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page := pageSnapshot{Oldest: resp.Header.Get("X-Codeagent-Oldest-Seq"), HasMore: resp.Header.Get("X-Codeagent-Has-More-Older"), Truncated: resp.Header.Get("X-Codeagent-Replay-Truncated"), Fragment: resp.Header.Get("X-Codeagent-Turn-Fragment"), Seqs: []int64{}, Kinds: []string{}}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, page
	}
	var frames []struct {
		Seq  int64  `json:"seq"`
		Kind string `json:"kind"`
	}
	decodeResponse(t, resp, &frames)
	for _, frame := range frames {
		page.Seqs = append(page.Seqs, frame.Seq)
		page.Kinds = append(page.Kinds, frame.Kind)
	}
	return resp.StatusCode, page
}

func TestConversationHistoryPaginationGolden(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	id := "history"
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	add := func(turn string, kind agent.EventKind) {
		t.Helper()
		_, err := store.RecordEvent(ctx, storedEvent(agent.Event{Kind: kind, SessionID: id, TurnID: turn, At: at}))
		if err != nil {
			t.Fatal(err)
		}
	}
	add("", agent.EventThinking) // leading event, before the first turn
	add("t1", agent.EventTurnStarted)
	add("t1", agent.EventToolStarted)
	add("t1", agent.EventTurnFinished)
	add("t2", agent.EventTurnStarted)
	add("t2", agent.EventToolStarted)
	add("t2", agent.EventThinking)
	add("t2", agent.EventTurnFinished)
	add("t3", agent.EventTurnStarted)
	add("t3", agent.EventTurnFinished)

	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: "/tmp/history"}
	srv := httptest.NewServer(newTestMux(repo, &conversation.StoreEventAdapter{Store: store}))
	defer srv.Close()
	base := srv.URL + "/v1/conversations/" + id + "/events"

	paths := []string{"?tail=2", "?before=9&limit=2", "?before=5&limit=2", "?before=2&limit=2", "?before=1&limit=2"}
	got := make([]pageSnapshot, 0, len(paths))
	for _, path := range paths {
		status, page := readPage(t, base+path)
		if status != http.StatusOK {
			t.Fatalf("%s: status=%d", path, status)
		}
		got = append(got, page)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "history_pagination.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected []pageSnapshot
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		actual, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("history pages differ from golden:\n%s", actual)
	}
	status, since := readPage(t, base+"?since=5&limit=1")
	if status != http.StatusOK || len(since.Seqs) != 5 || since.Seqs[0] != 6 || since.Seqs[4] != 10 {
		t.Fatalf("since replay changed: status=%d page=%+v", status, since)
	}
	for _, path := range []string{"?before=9&since=0", "?before=0", "?before=x", "?tail=0", "?tail=x", "?tail=10001", "?limit=2", "?through_turn_id=t1&tail=2"} {
		status, _ := readPage(t, base+path)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400", path, status)
		}
	}
	status, _ = readPage(t, srv.URL+"/v1/conversations/missing/events?tail=2")
	if status != http.StatusNotFound {
		t.Fatalf("missing conversation status=%d", status)
	}
}

func TestConversationHistoryPaginationOversizedTurn(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id := "large"
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, ev := range []agent.Event{
		{Kind: agent.EventTurnStarted, SessionID: id, TurnID: "t1", At: at},
		{Kind: agent.EventToolStdout, SessionID: id, TurnID: "t1", At: at, Chunk: strings.Repeat("a", 4<<20)},
		{Kind: agent.EventToolStdout, SessionID: id, TurnID: "t1", At: at, Chunk: strings.Repeat("b", 4<<20)},
		{Kind: agent.EventToolStdout, SessionID: id, TurnID: "t1", At: at, Chunk: strings.Repeat("c", 4<<20)},
		{Kind: agent.EventTurnFinished, SessionID: id, TurnID: "t1", At: at},
	} {
		if _, err := store.RecordEvent(context.Background(), storedEvent(ev)); err != nil {
			t.Fatal(err)
		}
	}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id}
	srv := httptest.NewServer(newTestMux(repo, &conversation.StoreEventAdapter{Store: store}))
	defer srv.Close()
	base := srv.URL + "/v1/conversations/" + id + "/events"
	_, page := readPage(t, base+"?tail=2")
	if page.Oldest != "4" || page.HasMore != "1" || page.Truncated != "1" || page.Fragment != "1" || !reflect.DeepEqual(page.Seqs, []int64{4, 5}) {
		t.Fatalf("oversized turn page=%+v", page)
	}
	_, middle := readPage(t, base+"?before="+page.Oldest+"&limit=2")
	if middle.Oldest != "3" || middle.HasMore != "1" || middle.Truncated != "1" || middle.Fragment != "1" || !reflect.DeepEqual(middle.Seqs, []int64{3}) {
		t.Fatalf("middle turn fragment=%+v", middle)
	}
	_, older := readPage(t, base+"?before="+middle.Oldest+"&limit=2")
	if older.Oldest != "1" || older.HasMore != "0" || older.Truncated != "" || older.Fragment != "0" || !reflect.DeepEqual(older.Seqs, []int64{1, 2}) {
		t.Fatalf("older turn fragment=%+v", older)
	}
	merged := append(append(append([]int64(nil), older.Seqs...), middle.Seqs...), page.Seqs...)
	if !reflect.DeepEqual(merged, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("reassembled turn seqs=%v", merged)
	}
	_, legacy := readPage(t, base)
	if legacy.Truncated != "1" || legacy.Oldest != "" || legacy.HasMore != "" {
		t.Fatalf("legacy response changed: %+v", legacy)
	}
	_, since := readPage(t, base+"?since=1")
	if len(since.Seqs) != 4 || since.Seqs[0] != 2 || since.Seqs[3] != 5 {
		t.Fatalf("unbounded since response=%+v", since)
	}
}

func TestConversationHistoryPaginationLegacyStore(t *testing.T) {
	id := "legacy"
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id}
	base := &fakeEventStore{}
	_, _ = base.Append(context.Background(), storedEvent(agent.Event{Kind: agent.EventTurnStarted, SessionID: id, At: time.Now()}))
	srv := httptest.NewServer(newTestMux(repo, legacyEventStore{base: base}))
	defer srv.Close()
	path := srv.URL + "/v1/conversations/" + id + "/events"
	status, replay := readPage(t, path+"?since=0")
	if status != http.StatusOK || !reflect.DeepEqual(replay.Seqs, []int64{1}) {
		t.Fatalf("legacy replay status=%d page=%+v", status, replay)
	}
	status, _ = readPage(t, path+"?tail=1")
	if status != http.StatusNotImplemented {
		t.Fatalf("legacy pagination status=%d, want 501", status)
	}
	resp, err := http.Get(srv.URL + "/v1/runtime/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps runtimeCapabilitiesResponse
	decodeResponse(t, resp, &caps)
	if caps.Capabilities.HistoryPagination {
		t.Fatal("legacy store advertised history_pagination")
	}
}

func TestConversationHistoryPaginationAdapterWithoutNativePage(t *testing.T) {
	id := "legacy-adapter"
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id}
	store := session.NewMemoryStore()
	_, _ = store.RecordEvent(context.Background(), storedEvent(agent.Event{Kind: agent.EventTurnStarted, SessionID: id, At: time.Now()}))
	adapter := &conversation.StoreEventAdapter{Store: eventStoreWithoutPage{EventStore: store}}
	srv := httptest.NewServer(newTestMux(repo, adapter))
	defer srv.Close()
	status, _ := readPage(t, srv.URL+"/v1/conversations/"+id+"/events?tail=1")
	if status != http.StatusNotImplemented {
		t.Fatalf("non-native pagination status=%d, want 501", status)
	}
	resp, err := http.Get(srv.URL + "/v1/runtime/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps runtimeCapabilitiesResponse
	decodeResponse(t, resp, &caps)
	if caps.Capabilities.HistoryPagination {
		t.Fatal("adapter without native page advertised history_pagination")
	}
}

func TestConversationDetailCountsBeyondLegacyReplayBudget(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id := "detail-large"
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	add := func(kind agent.EventKind, turn string, offset time.Duration, chunk string) {
		t.Helper()
		if _, err := store.RecordEvent(context.Background(), storedEvent(agent.Event{Kind: kind, SessionID: id, TurnID: turn, At: at.Add(offset), Chunk: chunk})); err != nil {
			t.Fatal(err)
		}
	}
	add(agent.EventTurnStarted, "t1", 0, "")
	add(agent.EventToolStdout, "t1", time.Second, strings.Repeat("x", 9<<20))
	add(agent.EventTurnFinished, "t1", 2*time.Second, "")
	add(agent.EventTurnStarted, "t2", 3*time.Second, "")
	add(agent.EventTurnFinished, "t2", 4*time.Second, "")
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id}
	srv := httptest.NewServer(newTestMux(repo, &conversation.StoreEventAdapter{Store: store}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/conversations/" + id)
	if err != nil {
		t.Fatal(err)
	}
	var detail ConversationDetail
	decodeResponse(t, resp, &detail)
	if detail.TurnCount != 2 || detail.MessageCount != 4 || detail.CreatedAt != at.Format(rfc3339Millis) || detail.UpdatedAt != at.Add(4*time.Second).Format(rfc3339Millis) {
		t.Fatalf("detail from complete event metadata = %+v", detail)
	}
}
