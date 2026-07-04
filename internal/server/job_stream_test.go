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

	"github.com/coder/websocket"
)

// funcSubscriber adapts a Subscribe closure to server.Subscriber.
type funcSubscriber func() (<-chan agent.Event, func())

func (f funcSubscriber) Subscribe() (<-chan agent.Event, func()) { return f() }

// TestJobStreamHandlerStreamsOverRealSocket dials the read-only job stream and
// asserts hello + a live job event arrive as v1 frames, byte-identical in shape
// to the conversation stream (client condition (a)).
func TestJobStreamHandlerStreamsOverRealSocket(t *testing.T) {
	hub := newTestHub()
	h := &JobStreamHandler{
		Resolve: func(*http.Request) (Subscriber, error) {
			return funcSubscriber(hub.subscribe), nil
		},
		ServerName: "codeagent/test",
		Accept:     &websocket.AcceptOptions{InsecureSkipVerify: true},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// 1) hello
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	var hello map[string]any
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatalf("hello not JSON: %v", err)
	}
	if hello["type"] != "hello" || hello["protocol_version"].(float64) != 1 {
		t.Fatalf("first message is not hello: %s", data)
	}

	// 2) a live job event (session_id = the job id) arrives as a wire frame.
	hub.Emit(agent.Event{
		Kind: agent.EventJobOutput, SessionID: "job_1", Seq: 7,
		Chunk: "Cloning repository...\n",
	})
	_, data, err = c.Read(ctx)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("event not JSON: %v", err)
	}
	if ev["kind"] != "job_output" || ev["session_id"] != "job_1" {
		t.Errorf("unexpected frame: %s", data)
	}
	if ev["chunk"] != "Cloning repository...\n" {
		t.Errorf("chunk missing: %s", data)
	}
	if seq, _ := ev["seq"].(float64); seq != 7 {
		t.Errorf("seq = %v, want 7 (child-partition cursor)", ev["seq"])
	}

	c.Close(websocket.StatusNormalClosure, "")
}

// TestJobStreamHandlerRejectsUnknown returns 404 (before upgrade) when the job
// id resolves to nothing.
func TestJobStreamHandlerRejectsUnknown(t *testing.T) {
	h := &JobStreamHandler{
		Resolve: func(*http.Request) (Subscriber, error) {
			return nil, http.ErrNoLocation
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
