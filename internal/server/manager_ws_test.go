package server

import (
	"context"
	"encoding/json"
	"fmt"
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

// oneRunnerFactory resumes a conversation around a single fixed runner, so the
// test can emit through that runner and watch frames arrive at the client.
type oneRunnerFactory struct{ runner *agent.Runner }

func (f oneRunnerFactory) Create(context.Context) (*conversation.Conversation, error) {
	return conversation.New(f.runner, &session.Session{ID: "new"}, nil), nil
}

func (f oneRunnerFactory) Resume(_ context.Context, id string) (*conversation.Conversation, error) {
	return conversation.New(f.runner, &session.Session{ID: id}, nil), nil
}

// TestWSHandlerResolvesViaManager proves the Manager fills the Resolve seam:
// a connection to /v1/conversations/{id} is routed to that conversation, and a
// server-side emit on its runner reaches the client as a frame.
func TestWSHandlerResolvesViaManager(t *testing.T) {
	runner := &agent.Runner{}
	mgr := conversation.NewManager(oneRunnerFactory{runner: runner})
	if _, err := mgr.Resume(context.Background(), "sess_root"); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown()

	h := &WSHandler{
		Resolve: func(r *http.Request) (Session, error) {
			id := strings.TrimPrefix(r.URL.Path, "/v1/conversations/")
			c, ok := mgr.Get(id)
			if !ok {
				return nil, fmt.Errorf("no such conversation %q", id)
			}
			return c, nil
		},
		ServerName: "codeagent/test",
		Accept:     &websocket.AcceptOptions{InsecureSkipVerify: true},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/conversations/sess_root"
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if _, data, err := c.Read(ctx); err != nil { // hello => subscribed
		t.Fatalf("read hello: %v", err)
	} else {
		var hello map[string]any
		_ = json.Unmarshal(data, &hello)
		if hello["type"] != "hello" {
			t.Fatalf("first message not hello: %s", data)
		}
	}

	runner.Emitter.Emit(agent.Event{Kind: agent.EventThinking, SessionID: "sess_root", Text: "routed"})

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev["kind"] != "thinking" || ev["text"] != "routed" {
		t.Errorf("event not routed through the resolved conversation: %s", data)
	}

	c.Close(websocket.StatusNormalClosure, "")
}

// TestWSHandlerUnknownConversation returns 404 when the manager has no such id.
func TestWSHandlerUnknownConversation(t *testing.T) {
	mgr := conversation.NewManager(oneRunnerFactory{runner: &agent.Runner{}})
	h := &WSHandler{
		Resolve: func(r *http.Request) (Session, error) {
			id := strings.TrimPrefix(r.URL.Path, "/v1/conversations/")
			c, ok := mgr.Get(id)
			if !ok {
				return nil, fmt.Errorf("no such conversation %q", id)
			}
			return c, nil
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
