//go:build unix

package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type reattachRuntime struct {
	mu                             sync.Mutex
	root, server, store, workspace string
	events                         []Event
	pending                        bool
	truncated                      bool
	inputs                         int
	conn                           *websocket.Conn
}

func reattachFixture(t *testing.T) (*reattachRuntime, string, string) {
	t.Helper()
	root := testRepo(t)
	f := &reattachRuntime{root: root, workspace: root, server: "server-1", store: "store-1", events: []Event{
		{Kind: "turn_accepted", SessionID: "s1", TurnID: "old1", RequestID: "old-req1", Seq: 1},
		{Kind: "turn_finished", SessionID: "s1", TurnID: "old1", Seq: 2},
		{Kind: "turn_accepted", SessionID: "s1", TurnID: "old2", RequestID: "old-req2", Seq: 3},
		{Kind: "turn_cancelled", SessionID: "s1", TurnID: "old2", Seq: 4},
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		conn := f.conn
		f.mu.Unlock()
		if conn != nil {
			_ = conn.CloseNow()
		}
		srv.Close()
	})
	path, _ := writeGrant(t, srv.URL)
	head, _, err := gitOutput(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "user-work.txt"), []byte("preserve me\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f, path, strings.TrimSpace(head)
}

func (f *reattachRuntime) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("fixture-token", 4) {
		w.WriteHeader(401)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/stream") {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		f.mu.Lock()
		f.conn = conn
		pending := f.pending
		f.mu.Unlock()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello"}`))
		if pending {
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"approval_request","id":"human1","session_id":"s1","turn_id":"old2","tool_name":"edit_file"}`))
		}
		for {
			_, b, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var in map[string]string
			_ = json.Unmarshal(b, &in)
			f.mu.Lock()
			f.inputs++
			f.events = append(f.events, Event{Kind: "turn_accepted", SessionID: "s1", TurnID: "new3", RequestID: in["request_id"], Seq: 5}, Event{Kind: "turn_finished", SessionID: "s1", TurnID: "new3", Seq: 6})
			f.mu.Unlock()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out any
	switch r.URL.Path {
	case "/v1/runtime/info", "/v1/tasks/capabilities":
		out = map[string]string{"server_id": f.server, "store_id": f.store}
	case "/v1/conversations/s1":
		out = map[string]any{"id": "s1", "workspace_path": f.workspace, "execution_policy": "shared_workspace"}
	case "/v1/conversations/s1/events":
		if f.truncated {
			w.Header().Set("X-Codeagent-Replay-Truncated", "1")
		}
		out = f.events
	case "/v1/activity":
		state, pending := "done", 0
		if f.pending {
			state, pending = "waiting_approval", 1
		}
		out = map[string]any{"is_delta": false, "sessions": []any{map[string]any{"session_id": "s1", "state": state, "pending_approval_count": pending, "pending_client_tool_count": 0}}}
	default:
		w.WriteHeader(404)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func TestExistingSessionReattachRestartAndTCPDisconnect(t *testing.T) {
	f, path, baseline := reattachFixture(t)
	ctx := context.Background()
	open := func() *Client {
		t.Helper()
		c, _, err := NewReattachedFromManifest(ctx, path, f.root, "s1", baseline)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	c := open()
	if _, err := c.Send(ctx, "s1", "old-req1", "never replay old input", ""); err == nil {
		t.Fatal("old request replayed")
	}
	r, err := c.Wait(ctx, "s1", "old2", 99, 0)
	if err != nil || r.Status != "cancelled" || r.EvidenceNote == "" {
		t.Fatalf("%+v %v", r, err)
	}
	// Close a real loopback WebSocket TCP connection, without stopping the HTTP
	// runtime or altering global networking. Durable HTTP replay remains usable.
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	_ = conn.CloseNow()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		gone := c.conn == nil
		c.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect not observed")
		}
		time.Sleep(time.Millisecond)
	}
	r, err = c.Read(ctx, "s1", "old1", 0)
	if err != nil || r.Status != "completed" {
		t.Fatalf("%+v %v", r, err)
	}
	c.Close()
	c = open()
	f.mu.Lock()
	inputs := f.inputs
	f.mu.Unlock()
	if inputs != 0 {
		t.Fatal("reattach submitted input or control")
	}
	if _, err = c.Send(ctx, "s1", "fresh-review", "scripted fixture only", ""); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	inputs = f.inputs
	f.mu.Unlock()
	if inputs != 1 {
		t.Fatal("unexpected input count")
	}
	b, err := os.ReadFile(filepath.Join(f.root, "user-work.txt"))
	if err != nil || string(b) != "preserve me\n" {
		t.Fatal("dirty user work changed")
	}
}

func TestReattachPendingHumanNeverApprovesOrResubmits(t *testing.T) {
	f, path, baseline := reattachFixture(t)
	f.pending = true
	f.events = f.events[:3]
	c, _, err := NewReattachedFromManifest(context.Background(), path, f.root, "s1", baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := c.Wait(context.Background(), "s1", "old2", 3, time.Second)
	if err != nil || r.Terminal || !r.AttentionRequired || r.Status != "waiting_for_human" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err = c.Send(context.Background(), "s1", "new-work", "must not run", ""); err == nil {
		t.Fatal("new work started during human attention")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs != 0 {
		t.Fatal("reattach approved or submitted")
	}
}

func TestReattachRejectsIdentityHistoryBaselineAndReplacedWorkspace(t *testing.T) {
	for _, name := range []string{"server", "store", "workspace", "baseline", "missing-admission", "duplicate-admission", "foreign-session", "unordered", "truncated"} {
		t.Run(name, func(t *testing.T) {
			f, path, baseline := reattachFixture(t)
			switch name {
			case "server":
				f.server = "wrong"
			case "store":
				f.store = "wrong"
			case "workspace":
				f.workspace += "/other"
			case "baseline":
				baseline = strings.Repeat("0", 40)
			case "missing-admission":
				f.events = f.events[1:]
			case "duplicate-admission":
				f.events[2].RequestID = "old-req1"
			case "foreign-session":
				f.events[1].SessionID = "foreign"
			case "unordered":
				f.events[1].Seq = 1
			case "truncated":
				f.truncated = true
			}
			if c, _, err := NewReattachedFromManifest(context.Background(), path, f.root, "s1", baseline); err == nil {
				c.Close()
				t.Fatal("unsafe reattachment accepted")
			}
		})
	}
	f, path, baseline := reattachFixture(t)
	c, _, err := NewReattachedFromManifest(context.Background(), path, f.root, "s1", baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = os.Rename(f.root, f.root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Read(context.Background(), "s1", "old1", 0); err == nil {
		t.Fatal("replacement workspace accepted")
	}
}

func TestReattachRefusesWorkAndArtifactsAfterAnotherClientStartsTurn(t *testing.T) {
	f, path, baseline := reattachFixture(t)
	c, _, err := NewReattachedFromManifest(context.Background(), path, f.root, "s1", baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f.mu.Lock()
	f.events = append(f.events, Event{Kind: "turn_accepted", SessionID: "s1", TurnID: "other3", RequestID: "other-client", Seq: 5})
	f.mu.Unlock()
	if _, err = c.Send(context.Background(), "s1", "new", "do not overlap", ""); err == nil {
		t.Fatal("overlapping writer admitted")
	}
	if _, err = c.Artifacts(context.Background(), "s1", "old1"); err == nil {
		t.Fatal("artifacts collected during other client's active turn")
	}
	if _, err = c.Read(context.Background(), "s1", "missing-turn", 0); err == nil {
		t.Fatal("unknown exact Turn accepted")
	}
}
