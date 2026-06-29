package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

// ---- test adapters for ConversationRepository ----

// fakeConversationRepo is an in-memory ConversationRepository for mux tests.
type fakeConversationRepo struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
}

func newFakeConversationRepo() *fakeConversationRepo {
	return &fakeConversationRepo{sessions: make(map[string]*session.Session)}
}

func (r *fakeConversationRepo) Create(ctx context.Context, workspacePath string) (*session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &session.Session{ID: fmt.Sprintf("sess_%d", len(r.sessions)+1), WorkspacePath: workspacePath}
	r.sessions[s.ID] = s
	return s, nil
}

func (r *fakeConversationRepo) Load(ctx context.Context, id string) (*session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return s, nil
}

func (r *fakeConversationRepo) Save(ctx context.Context, s *session.Session) error { return nil }
func (r *fakeConversationRepo) List(ctx context.Context) ([]session.Meta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []session.Meta
	for _, s := range r.sessions {
		out = append(out, session.Meta{ID: s.ID, WorkspacePath: s.WorkspacePath})
	}
	return out, nil
}
func (r *fakeConversationRepo) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
	return nil
}
func (r *fakeConversationRepo) UpdateName(ctx context.Context, id string, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.Name = name
	return nil
}
func (r *fakeConversationRepo) Close() error { return nil }

// ---- test adapters for ConversationEventStore ----

// fakeEventStore implements ConversationEventStore with canned data.
type fakeEventStore struct {
	mu   sync.Mutex
	recs map[string][]session.EventRecord
}

func (s *fakeEventStore) Append(ctx context.Context, e session.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recs == nil {
		s.recs = make(map[string][]session.EventRecord)
	}
	s.recs[e.SessionID] = append(s.recs[e.SessionID], e)
	return nil
}

func (s *fakeEventStore) Replay(ctx context.Context, sessionID string) ([]session.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recs[sessionID], nil
}

func storedEvent(ev agent.Event) session.EventRecord {
	p, _ := json.Marshal(ev)
	return session.EventRecord{SessionID: ev.SessionID, TurnID: ev.TurnID, Kind: string(ev.Kind), At: ev.At, Payload: p}
}

// newTestMux returns an mux wired with test fakes. executor is nil → WS endpoint
// is skipped.
func newTestMux(repo conversation.ConversationRepository, events conversation.ConversationEventStore) http.Handler {
	return NewMux(repo, events, nil, MuxOptions{})
}

func TestMuxHealthz(t *testing.T) {
	srv := httptest.NewServer(newTestMux(newFakeConversationRepo(), &fakeEventStore{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMuxCreateThenList(t *testing.T) {
	repo := newFakeConversationRepo()
	srv := httptest.NewServer(newTestMux(repo, &fakeEventStore{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/conversations", "application/json",
		strings.NewReader(`{"workspace_path":"/Users/x/proj"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var ref ConversationRef
	if err := json.NewDecoder(resp.Body).Decode(&ref); err != nil {
		t.Fatal(err)
	}
	if ref.ID == "" {
		t.Fatal("create did not return an id")
	}
	if ref.WorkspacePath != "/Users/x/proj" {
		t.Errorf("WorkspacePath = %q", ref.WorkspacePath)
	}

	// List should include the created conversation.
	resp2, err := http.Get(srv.URL + "/v1/conversations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var refs []ConversationRef
	if err := json.NewDecoder(resp2.Body).Decode(&refs); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range refs {
		if r.ID == ref.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("created conversation %q not in list %+v", ref.ID, refs)
	}
}

func TestMuxCreateAcceptsEmptyBody(t *testing.T) {
	repo := newFakeConversationRepo()
	srv := httptest.NewServer(newTestMux(repo, &fakeEventStore{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/conversations", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("create with empty body status = %d, want 201", resp.StatusCode)
	}
}

func TestMuxDelete(t *testing.T) {
	repo := newFakeConversationRepo()
	srv := httptest.NewServer(newTestMux(repo, &fakeEventStore{}))
	defer srv.Close()

	// Create then delete.
	resp, _ := http.Post(srv.URL+"/v1/conversations", "application/json", nil)
	var ref ConversationRef
	json.NewDecoder(resp.Body).Decode(&ref)
	resp.Body.Close()

	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/conversations/"+ref.ID, nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", resp2.StatusCode)
	}
	resp2.Body.Close()

	// List should be empty.
	resp3, _ := http.Get(srv.URL + "/v1/conversations")
	var refs []ConversationRef
	json.NewDecoder(resp3.Body).Decode(&refs)
	resp3.Body.Close()
	if len(refs) != 0 {
		t.Errorf("list after delete = %d, want 0", len(refs))
	}
}

// ---- conversation read API (P1-A.5) ----

func readMuxWithHistory() (http.Handler, string) {
	at := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	id := "sess_hist"
	recs := []session.EventRecord{
		storedEvent(agent.Event{Kind: agent.EventTurnStarted, SessionID: id, TurnID: "t1", At: at, Text: "分析项目"}),
		storedEvent(agent.Event{Kind: agent.EventToolStarted, SessionID: id, TurnID: "t1", At: at.Add(time.Second), Step: 1, ToolName: "grep", ToolArgs: `{"q":"x"}`}),
		storedEvent(agent.Event{Kind: agent.EventModelFinished, SessionID: id, TurnID: "t1", At: at.Add(2 * time.Second), PromptTokens: 100, Elapsed: 731 * time.Millisecond}),
		storedEvent(agent.Event{Kind: agent.EventTurnFinished, SessionID: id, TurnID: "t1", At: at.Add(3 * time.Second), Text: "项目结构如下"}),
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{id: recs}}

	repo := newFakeConversationRepo()
	// Pre-populate the session so Load succeeds.
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: "/tmp/test"}

	mux := newTestMux(repo, events)
	return mux, id
}

func TestMuxGetEventsReEncodesToWire(t *testing.T) {
	mux, id := readMuxWithHistory()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var frames []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&frames); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4 {
		t.Fatalf("want 4 event frames, got %d", len(frames))
	}
	tool := frames[1]
	if tool["kind"] != "tool_started" {
		t.Fatalf("frame[1] kind = %v", tool["kind"])
	}
	if args, _ := tool["tool_args"].(map[string]any); args["q"] != "x" {
		t.Errorf("tool_args not structured JSON in history: %v", tool["tool_args"])
	}
	if frames[2]["elapsed_ms"].(float64) != 731 {
		t.Errorf("elapsed_ms = %v, want 731", frames[2]["elapsed_ms"])
	}
}

func TestMuxGetMessagesDerivesFromEvents(t *testing.T) {
	mux, id := readMuxWithHistory()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msgs []MessageView
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (user+assistant), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "分析项目" {
		t.Errorf("msg[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "项目结构如下" {
		t.Errorf("msg[1] = %+v", msgs[1])
	}
}

func TestMuxGetDetail(t *testing.T) {
	mux, id := readMuxWithHistory()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d ConversationDetail
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.ID != id || d.TurnCount != 1 || d.MessageCount != 2 {
		t.Errorf("detail = %+v", d)
	}
	if d.CreatedAt == "" || d.UpdatedAt == "" {
		t.Errorf("detail missing timestamps: %+v", d)
	}
}

func TestMuxReadUnknownConversation404(t *testing.T) {
	srv := httptest.NewServer(newTestMux(newFakeConversationRepo(), &fakeEventStore{}))
	defer srv.Close()

	for _, path := range []string{"/v1/conversations/nope", "/v1/conversations/nope/messages", "/v1/conversations/nope/events"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}
}
