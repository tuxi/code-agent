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

	"github.com/coder/websocket"
)

// approvalSession is a fake Session whose SendMessage triggers one side-effect
// approval and emits the verdict, so the test can drive the full inbound
// round-trip over a real socket without a model.
type approvalSession struct {
	events chan agent.Event
	mu     sync.Mutex
	appr   agent.Approver
}

func (s *approvalSession) Subscribe() (<-chan agent.Event, func()) { return s.events, func() {} }
func (s *approvalSession) Cancel()                                 {}

func (s *approvalSession) SetApprover(a agent.Approver) {
	s.mu.Lock()
	s.appr = a
	s.mu.Unlock()
}

func (s *approvalSession) SetPlanApprover(agent.PlanApprover) {}

func (s *approvalSession) SendMessage(context.Context, string) (agent.TurnResult, error) {
	s.mu.Lock()
	a := s.appr
	s.mu.Unlock()
	approved := a.Approve("run_command", json.RawMessage(`{"command":"git push"}`))
	s.events <- agent.Event{Kind: agent.EventTurnFinished, Text: fmt.Sprintf("approved=%v", approved)}
	return agent.TurnResult{}, nil
}

// TestWSInboundApprovalRoundTrip is the headline proof of the inbound plane: a
// client drives a turn, the turn's side-effecting tool blocks on the remote
// approver, the client approves over the wire, and the verdict flows back.
func TestWSInboundApprovalRoundTrip(t *testing.T) {
	sess := &approvalSession{events: make(chan agent.Event, 8)}
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

	readType(t, ctx, c, "hello") // handshake => bridge subscribed + approver attached

	wsWriteJSON(t, ctx, c, map[string]any{"type": "send_message", "text": "go"})

	// The side-effecting tool produces an approval_request.
	req := readFrame(t, ctx, c)
	if req["type"] != "approval_request" || req["tool_name"] != "run_command" {
		t.Fatalf("expected approval_request, got %s", mustJSON(req))
	}
	if args, _ := req["tool_args"].(map[string]any); args["command"] != "git push" {
		t.Errorf("approval_request tool_args not structured JSON: %s", mustJSON(req))
	}
	id, _ := req["id"].(string)
	if id == "" {
		t.Fatal("approval_request missing id")
	}

	wsWriteJSON(t, ctx, c, map[string]any{"type": "approval_response", "id": id, "approved": true})

	fin := readFrame(t, ctx, c)
	if fin["kind"] != "turn_finished" || fin["text"] != "approved=true" {
		t.Errorf("verdict did not flow back through the blocking approver: %s", mustJSON(fin))
	}

	c.Close(websocket.StatusNormalClosure, "")
}

func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("frame is not JSON: %v", err)
	}
	return m
}

func readType(t *testing.T, ctx context.Context, c *websocket.Conn, typ string) map[string]any {
	t.Helper()
	m := readFrame(t, ctx, c)
	if m["type"] != typ {
		t.Fatalf("want type %q, got %s", typ, mustJSON(m))
	}
	return m
}

func wsWriteJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	data, _ := json.Marshal(v)
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
