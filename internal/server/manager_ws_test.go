package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"code-agent/internal/agent"

	"github.com/coder/websocket"
)

// TestWSHandlerResolvesViaTransportSession proves the TurnExecutor/TransportSession
// fills the Resolve seam: a connection to /v1/conversations/{id}/stream is
// routed to a TransportSession sharing a test hub, and a hub-emitted event
// reaches the client as a frame.
func TestWSHandlerResolvesViaTransportSession(t *testing.T) {
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

	url := httptestServerToWS(srv.URL)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if _, data, err := c.Read(ctx); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	} else {
		var hello map[string]any
		_ = json.Unmarshal(data, &hello)
		if hello["type"] != "hello" {
			t.Fatalf("first message not hello: %s", data)
		}
	}

	hub.Emit(agent.Event{Kind: agent.EventThinking, SessionID: "sess_root", Text: "routed"})

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev["kind"] != "thinking" || ev["text"] != "routed" {
		t.Errorf("event not routed through TransportSession: %s", data)
	}

	c.Close(websocket.StatusNormalClosure, "")
}

// TestWSHandlerUnknownConversation returns 404 when the resolve func errors.
func TestWSHandlerUnknownConversation(t *testing.T) {
	h := &WSHandler{
		Resolve: func(r *http.Request) (Session, error) {
			return nil, fmt.Errorf("no such conversation %q", "missing")
		},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func httptestServerToWS(httpURL string) string {
	return "ws" + httpURL[4:]
}
