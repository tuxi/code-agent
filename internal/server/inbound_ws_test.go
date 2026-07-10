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

func (s *approvalSession) SetPlanApprover(agent.PlanApprover)         {}
func (s *approvalSession) SetClientToolWaiter(agent.ClientToolWaiter) {}
func (s *approvalSession) RegisterTools([]agent.ClientToolDef)        {}

func (s *approvalSession) SendMessage(context.Context, string, string) (agent.TurnResult, error) {
	s.mu.Lock()
	a := s.appr
	s.mu.Unlock()
	approved := a.Approve("run_command", json.RawMessage(`{"command":"git push"}`))
	s.events <- agent.Event{Kind: agent.EventTurnFinished, Text: fmt.Sprintf("approved=%v", approved == agent.VerdictAllow)}
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

// clientToolSession is a fake Session whose SendMessage triggers a client-side
// tool call: it emits tool_started(executor:"client"), blocks on the waiter,
// then emits tool_finished + turn_finished. Used to test the full tool_result
// round-trip over a real WebSocket.
type clientToolSession struct {
	events chan agent.Event
	mu     sync.Mutex
	w      agent.ClientToolWaiter
}

func (s *clientToolSession) Subscribe() (<-chan agent.Event, func()) { return s.events, func() {} }
func (s *clientToolSession) Cancel()                                 {}

func (s *clientToolSession) SetApprover(agent.Approver)         {}
func (s *clientToolSession) SetPlanApprover(agent.PlanApprover) {}
func (s *clientToolSession) SetClientToolWaiter(w agent.ClientToolWaiter) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

func (s *clientToolSession) RegisterTools([]agent.ClientToolDef) {}

func (s *clientToolSession) SendMessage(ctx context.Context, _ string, _ string) (agent.TurnResult, error) {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()

	// Emit tool_started with executor:"client" — exactly what a real client tool would produce.
	s.events <- agent.Event{
		Kind: agent.EventToolStarted, SessionID: "sess_root", TurnID: "turn_1",
		CallID: "call_99", Step: 1, ToolName: "trim_video",
		ToolArgs: `{"url":"file:///tmp/input.mp4"}`,
		Executor: "client",
	}

	if w != nil {
		// Block until the client delivers the result.
		result, waitErr := w.Wait(ctx, "call_99", 5*time.Second)
		if waitErr != nil {
			s.events <- agent.Event{
				Kind: agent.EventToolFinished, SessionID: "sess_root", TurnID: "turn_1",
				CallID: "call_99", Step: 1, ToolName: "trim_video",
				Observation: "Tool error: " + waitErr.Error(),
				Err:         waitErr.Error(),
			}
		} else if result.IsError {
			s.events <- agent.Event{
				Kind: agent.EventToolFinished, SessionID: "sess_root", TurnID: "turn_1",
				CallID: "call_99", Step: 1, ToolName: "trim_video",
				Observation: "Tool error: " + result.Content,
				Err:         result.Content,
			}
		} else {
			s.events <- agent.Event{
				Kind: agent.EventToolFinished, SessionID: "sess_root", TurnID: "turn_1",
				CallID: "call_99", Step: 1, ToolName: "trim_video",
				Observation: result.Content,
			}
		}
	} else {
		// No client connected — tool error.
		s.events <- agent.Event{
			Kind: agent.EventToolFinished, SessionID: "sess_root", TurnID: "turn_1",
			CallID: "call_99", Step: 1, ToolName: "trim_video",
			Observation: "Tool error: no client connected",
			Err:         "no client connected",
		}
	}

	s.events <- agent.Event{
		Kind: agent.EventTurnFinished, SessionID: "sess_root", TurnID: "turn_1",
		Text: "视频修剪完成",
	}
	return agent.TurnResult{Final: "视频修剪完成"}, nil
}

// TestWSInboundClientToolRoundTrip is the integration proof of the client tool
// execution round-trip: a client triggers a turn, the turn emits a client-side
// tool_started and blocks on the waiter, the client sends agent_input(kind:
// "tool_result") to unblock it, and the turn finishes.
func TestWSInboundClientToolRoundTrip(t *testing.T) {
	sess := &clientToolSession{events: make(chan agent.Event, 8)}
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

	readType(t, ctx, c, "hello") // handshake → bridge subscribed + waiter attached

	// Step 1: Start a turn. The fake session will emit tool_started(executor:"client")
	// and block on the waiter.
	wsWriteJSON(t, ctx, c, map[string]any{"type": "agent_input", "kind": "text", "text": "修剪视频"})

	// Step 2: Read tool_started. Verify it has executor:"client".
	ts := readFrame(t, ctx, c)
	if ts["kind"] != "tool_started" {
		t.Fatalf("want tool_started, got %s", mustJSON(ts))
	}
	if ts["tool_name"] != "trim_video" {
		t.Errorf("tool_name = %v, want trim_video", ts["tool_name"])
	}
	if ts["executor"] != "client" {
		t.Fatalf("executor = %v, want client", ts["executor"])
	}
	callID, _ := ts["call_id"].(string)
	if callID == "" {
		t.Fatal("tool_started missing call_id")
	}

	// Step 3: Send the tool_result back. This must unblock the waiter.
	wsWriteJSON(t, ctx, c, map[string]any{
		"type": "agent_input",
		"kind": "tool_result",
		"tool_result": map[string]any{
			"tool_use_id": callID,
			"subtype":     "result",
			"content":     "修剪完成: /tmp/output.mp4",
			"is_error":    false,
		},
	})

	// Step 4: Read tool_finished. Verify the observation matches.
	tf := readFrame(t, ctx, c)
	if tf["kind"] != "tool_finished" {
		t.Fatalf("want tool_finished, got %s", mustJSON(tf))
	}
	if tf["observation"] != "修剪完成: /tmp/output.mp4" {
		t.Errorf("observation = %v, want 修剪完成: /tmp/output.mp4", tf["observation"])
	}

	// Step 5: Read turn_finished.
	fin := readFrame(t, ctx, c)
	if fin["kind"] != "turn_finished" || fin["text"] != "视频修剪完成" {
		t.Errorf("want turn_finished, got %s", mustJSON(fin))
	}

	c.Close(websocket.StatusNormalClosure, "")
}
