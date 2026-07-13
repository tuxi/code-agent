package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/approve"

	"github.com/coder/websocket"
)

type controlTestSink struct{ id string }

func (s *controlTestSink) Send([]byte) error { return nil }

type controlApprovalTarget struct{ calls int }

func (t *controlApprovalTarget) Resolve(string, bool)                          { t.calls++ }
func (t *controlApprovalTarget) ResolveTool(string, bool, bool, approve.Scope) { t.calls++ }

type controlToolTarget struct{ calls int }

func (t *controlToolTarget) Deliver(string, agent.ToolCallResult) { t.calls++ }

// testSession is a minimal server.Session used by WS tests. It wraps a simple
// in-process subscription hub — no conversation package dependency.
type testSession struct {
	hub *testHub
}

func TestWSHandlerReusesClientToolWaiterPerSession(t *testing.T) {
	h := &WSHandler{}
	first := h.ensureToolWaiter("session_a")
	if got := h.ensureToolWaiter("session_a"); got != first {
		t.Fatal("same session received a replacement waiter on reconnect")
	}
	if other := h.ensureToolWaiter("session_b"); other == first {
		t.Fatal("different sessions shared a client-tool waiter")
	}

	result := make(chan agent.ToolCallResult, 1)
	go func() {
		got, _ := first.Wait(context.Background(), "call_1", time.Second)
		result <- got
	}()
	time.Sleep(20 * time.Millisecond) // wait for the broker to register call_1
	// Deliver through the waiter recovered by a simulated reconnect.
	h.ensureToolWaiter("session_a").Deliver("call_1", agent.ToolCallResult{Subtype: "result", Content: "done"})
	select {
	case got := <-result:
		if got.Content != "done" {
			t.Fatalf("result = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("pending client tool did not survive reconnect")
	}
}

func TestWSHandlerLatestConnectionOwnsSessionControl(t *testing.T) {
	h := &WSHandler{}
	firstSink := &controlTestSink{id: "first"}
	approver, waiter, firstRevision := h.claimSessionControl("session", firstSink)
	secondSink := &controlTestSink{id: "second"}
	_, _, secondRevision := h.claimSessionControl("session", secondSink)
	if secondRevision <= firstRevision {
		t.Fatalf("revisions first=%d second=%d", firstRevision, secondRevision)
	}

	approvalTarget := &controlApprovalTarget{}
	oldApproval := revisionApprovalResolver{handler: h, sessionID: "session", revision: firstRevision, target: approvalTarget}
	newApproval := revisionApprovalResolver{handler: h, sessionID: "session", revision: secondRevision, target: approvalTarget}
	oldApproval.Resolve("approval", true)
	newApproval.ResolveTool("approval", true, false, approve.ScopeUser)
	if approvalTarget.calls != 1 {
		t.Fatalf("approval deliveries=%d want only current owner", approvalTarget.calls)
	}

	toolTarget := &controlToolTarget{}
	oldTool := revisionToolResultResolver{handler: h, sessionID: "session", revision: firstRevision, target: toolTarget}
	newTool := revisionToolResultResolver{handler: h, sessionID: "session", revision: secondRevision, target: toolTarget}
	oldTool.Deliver("call", agent.ToolCallResult{Content: "stale"})
	newTool.Deliver("call", agent.ToolCallResult{Content: "current"})
	if toolTarget.calls != 1 {
		t.Fatalf("tool deliveries=%d want only current owner", toolTarget.calls)
	}

	// A late disconnect from the old socket must not clear the replacement sink.
	h.releaseSessionControl("session", firstRevision, approver)
	approver.mu.Lock()
	gotSink := approver.sink
	approver.mu.Unlock()
	if gotSink != secondSink {
		t.Fatal("old connection cleared or replaced the current control sink")
	}
	if h.ensureToolWaiter("session") != waiter {
		t.Fatal("control claim replaced the session tool broker")
	}
}

func (s *testSession) Subscribe() (<-chan agent.Event, func()) {
	return s.hub.subscribe()
}
func (s *testSession) SendMessage(context.Context, string, string) (agent.TurnResult, error) {
	return agent.TurnResult{}, nil
}
func (s *testSession) Cancel()                                    {}
func (s *testSession) SetApprover(agent.Approver)                 {}
func (s *testSession) SetPlanApprover(agent.PlanApprover)         {}
func (s *testSession) SetClientToolWaiter(agent.ClientToolWaiter) {}
func (s *testSession) RegisterTools([]agent.ClientToolDef)        {}

// testHub is the same shape as the old hub: Emit fans to subscribers.
type testHub struct {
	subs map[int]chan agent.Event
	next int
}

func newTestHub() *testHub { return &testHub{subs: make(map[int]chan agent.Event)} }

func (h *testHub) Emit(e agent.Event) {
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (h *testHub) subscribe() (<-chan agent.Event, func()) {
	ch := make(chan agent.Event, 256)
	id := h.next
	h.next++
	h.subs[id] = ch
	return ch, func() {
		delete(h.subs, id)
		close(ch)
	}
}

// TestWSHandlerStreamsOverRealSocket dials a real WebSocket against the handler
// and asserts the full path: hello handshake, then a core event emitted
// server-side arrives at the client as a v1 JSON frame.
func TestWSHandlerStreamsOverRealSocket(t *testing.T) {
	hub := newTestHub()
	sess := &testSession{hub: hub}

	h := &WSHandler{
		Resolve:    func(*http.Request) (Session, error) { return sess, nil },
		ServerName: "codeagent/test",
		Accept:     &websocket.AcceptOptions{InsecureSkipVerify: true},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// 1) hello — sent after the bridge subscribed.
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	var hello map[string]any
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatalf("hello not JSON: %v", err)
	}
	if hello["type"] != "hello" || hello["protocol_version"].(float64) != 1 {
		t.Fatalf("first message is not the hello handshake: %s", data)
	}

	// 2) emit a core event; the client must receive its encoded frame.
	hub.Emit(agent.Event{
		Kind: agent.EventTurnFinished, SessionID: "sess_root", TurnID: "turn_1",
		Text: "done",
	})

	_, data, err = c.Read(ctx)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("event not JSON: %v", err)
	}
	if ev["kind"] != "turn_finished" || ev["text"] != "done" {
		t.Errorf("unexpected event frame: %s", data)
	}
	if id, _ := ev["event_id"].(string); id == "" {
		t.Errorf("event_id was not stamped: %s", data)
	}

	c.Close(websocket.StatusNormalClosure, "")
}

// TestWSHandlerRejectsUnresolved returns 404 when the conversation cannot be
// resolved, before any upgrade.
func TestWSHandlerRejectsUnresolved(t *testing.T) {
	h := &WSHandler{
		Resolve: func(*http.Request) (Session, error) {
			return nil, http.ErrNoLocation // any error
		},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
