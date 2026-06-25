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
	"code-agent/internal/conversation"
	"code-agent/internal/session"

	"github.com/coder/websocket"
)

// TestWSHandlerStreamsOverRealSocket dials a real WebSocket against the handler
// and asserts the full path: hello handshake, then a core event emitted
// server-side arrives at the client as a v1 JSON frame.
func TestWSHandlerStreamsOverRealSocket(t *testing.T) {
	runner := &agent.Runner{}
	conv := conversation.New(runner, &session.Session{ID: "sess_root"}, nil)

	h := &WSHandler{
		Resolve:    func(*http.Request) (Session, error) { return conv, nil },
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

	// 1) hello — sent after the bridge subscribed, so a later emit is guaranteed
	// to reach this client.
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

	// 2) emit a core event the way the agent loop would; the client must receive
	// its encoded frame.
	runner.Emitter.Emit(agent.Event{
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
