package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Clone existing history instead of creating test commits or touching the
// caller's worktree. Every mutation below is inside t.TempDir.
func testRepo(t *testing.T) string {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "clone", "--quiet", "--shared", "--", strings.TrimSpace(string(root)), dest).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s %v", out, err)
	}
	canonical, err := filepath.EvalSymlinks(dest)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

type fakeRuntime struct {
	mu       sync.Mutex
	events   []Event
	inputs   int
	cancels  int
	approval bool
	root     string
}

func (f *fakeRuntime) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "denied", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/conversations":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["workspace_path"] != f.root {
			http.Error(w, "workspace", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(SessionRef{"s1", f.root})
	case strings.HasSuffix(r.URL.Path, "/events"):
		f.mu.Lock()
		defer f.mu.Unlock()
		// Deliberately return the durable stream even if live notifications are
		// absent. Admission and completion must not depend on the WS reader.
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		visible := []Event{}
		for _, event := range f.events {
			if event.Seq > since {
				visible = append(visible, event)
			}
		}
		_ = json.NewEncoder(w).Encode(visible)
	case r.URL.Path == "/v1/activity":
		f.mu.Lock()
		defer f.mu.Unlock()
		count := 0
		if f.approval {
			count = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{map[string]any{"session_id": "s1", "pending_approval_count": count}}})
	case strings.HasSuffix(r.URL.Path, "/stream"):
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello"}`))
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var in struct {
				Type, RequestID string
				Text            string
			}
			var envelope map[string]string
			_ = json.Unmarshal(raw, &envelope)
			in.Type = envelope["type"]
			in.RequestID = envelope["request_id"]
			f.mu.Lock()
			if in.Type == "cancel_turn" {
				f.cancels++
				f.events = append(f.events, Event{Kind: "turn_cancelled", SessionID: "s1", TurnID: "t1", Seq: 30})
				f.mu.Unlock()
				continue
			}
			f.inputs++
			f.events = append(f.events, Event{Kind: "turn_accepted", SessionID: "s1", TurnID: "t1", RequestID: in.RequestID, Seq: 10}, Event{Kind: "turn_started", SessionID: "s1", TurnID: "t1", Seq: 12})
			approval := f.approval
			f.mu.Unlock()
			if approval {
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"approval_request","id":"a1","tool_name":"edit_file"}`))
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func setup(t *testing.T) (*Client, *fakeRuntime) {
	t.Helper()
	root := testRepo(t)
	f := &fakeRuntime{root: root, events: []Event{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c, err := New(context.Background(), srv.URL, "test-token", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, f
}

func TestMCPDelegationAndEvidence(t *testing.T) {
	c, f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	ss, err := Server(c).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-codex-client"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, in any, out any) {
		t.Helper()
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s: %v", name, result.Content)
		}
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	var ref SessionRef
	call("codeagent_create_session", CreateInput{"create1"}, &ref)
	var receipt Receipt
	call("codeagent_send_to_session", SendInput{SessionID: ref.ID, RequestID: "req1", Message: "implement the task"}, &receipt)
	if !receipt.Accepted || receipt.Cursor != 10 || receipt.TurnID != "t1" {
		t.Fatalf("bad receipt: %+v", receipt)
	}
	call("codeagent_send_to_session", SendInput{SessionID: ref.ID, RequestID: "req1", Message: "implement the task"}, &receipt)
	if _, err := c.Send(ctx, "s1", "req1", "different payload", ""); err == nil {
		t.Fatal("payload conflict accepted")
	}
	f.mu.Lock()
	inputs := f.inputs
	f.mu.Unlock()
	if inputs != 1 {
		t.Fatalf("duplicate execution: %d", inputs)
	}
	var state ReadResult
	call("codeagent_wait_sessions", WaitInput{"s1", "t1", receipt.Cursor, 0}, &state)
	if terminal(state.Status) {
		t.Fatal("timeout manufactured completion")
	}
	if _, err := c.Artifacts(ctx, "s1", "t1"); err == nil {
		t.Fatal("collected artifacts during active turn")
	}
	if _, err := c.Read(ctx, "another-session", "t1", 0); err == nil {
		t.Fatal("foreign session accepted")
	}
	if _, err := c.Send(ctx, "s1", "req2", "parallel work", ""); err == nil {
		t.Fatal("parallel writer accepted")
	}
	if err := os.WriteFile(filepath.Join(c.root, "README.md"), []byte("changed tracked file\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.root, "new evidence.txt"), []byte("new file\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.events = append(f.events,
		Event{Kind: "tool_finished", SessionID: "s1", TurnID: "other-turn", Seq: 15, ToolName: "run_command", Observation: `{"command":"wrong turn","exit_code":0}`},
		Event{Kind: "tool_finished", SessionID: "s1", TurnID: "t1", Seq: 20, CallID: "test-failed", ToolName: "run_command", Observation: "[observation] failure=test\n---\n" + `{"command":"go test ./internal/example","exit_code":1,"stdout":"FAIL","decision":"allow"}`},
		Event{Kind: "tool_finished", SessionID: "s1", TurnID: "t1", Seq: 22, CallID: "background", ToolName: "run_command", Observation: `{"command":"go test","job_id":"job-1","status":"running"}`},
		Event{Kind: "turn_finished", SessionID: "s1", TurnID: "t1", Seq: 25, Text: "done"})
	f.mu.Unlock()
	call("codeagent_wait_sessions", WaitInput{"s1", "t1", receipt.Cursor, 1}, &state)
	if state.Status != "completed" || state.Cursor != 25 {
		t.Fatalf("wrong completion: %+v", state)
	}
	// A consumer passing back the last observed cursor must still see the
	// exact turn's durable terminal state, rather than waiting forever.
	state, err = c.Wait(ctx, "s1", "t1", 25, 0)
	if err != nil || state.Status != "completed" {
		t.Fatalf("terminal lost after cursor advance: %+v %v", state, err)
	}
	var evidence Artifacts
	call("codeagent_get_artifacts", ArtifactInput{"s1", "t1"}, &evidence)
	if !strings.Contains(evidence.Diff, "changed tracked file") || !strings.Contains(evidence.Diff, "new file") || evidence.Truncated {
		t.Fatalf("missing actual diff: %+v", evidence)
	}
	if len(evidence.CommandResults) != 1 || *evidence.CommandResults[0].ExitCode != 1 || len(evidence.UnparsedResults) != 1 {
		t.Fatalf("wrong command evidence: %+v", evidence)
	}
	if err := os.WriteFile(filepath.Join(c.root, "large-untracked.txt"), []byte(strings.Repeat("large diff line\n", maxGitOutput)), 0644); err != nil {
		t.Fatal(err)
	}
	evidence, err = c.Artifacts(ctx, "s1", "t1")
	if err != nil || !evidence.Truncated || len(evidence.Diff) > maxGitOutput {
		t.Fatalf("diff cap failed: len=%d truncated=%v err=%v", len(evidence.Diff), evidence.Truncated, err)
	}
}

func TestHumanApprovalAndCancellation(t *testing.T) {
	c, f := setup(t)
	f.approval = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, err := c.Create(ctx, "create")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.Send(ctx, ref.ID, "req", "edit", "")
	if err != nil {
		t.Fatal(err)
	}
	state, err := c.Wait(ctx, ref.ID, receipt.TurnID, receipt.Cursor, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "waiting_for_human" || len(state.PendingControls) != 1 {
		t.Fatalf("missing approval: %+v", state)
	}
	// Simulate a different UI device resolving the request. There is no
	// approval response emitted by this bridge.
	f.mu.Lock()
	f.approval = false
	f.mu.Unlock()
	state, err = c.Read(ctx, ref.ID, receipt.TurnID, 0)
	if err != nil || state.Status == "waiting_for_human" {
		t.Fatalf("stale approval: %+v %v", state, err)
	}
	if err = c.Cancel(ctx, ref.ID); err != nil {
		t.Fatal(err)
	}
	state, err = c.Wait(ctx, ref.ID, receipt.TurnID, receipt.Cursor, time.Second)
	if err != nil || state.Status != "cancelled" {
		t.Fatalf("cancel: %+v %v", state, err)
	}
}

func TestEndpointAndWorkspaceRestrictions(t *testing.T) {
	root := testRepo(t)
	for _, endpoint := range []string{"http://example.com:80", "http://localhost:8797", "http://127.0.0.1", "http://127.0.0.1:80/prefix", "http://user:secret@127.0.0.1:80"} {
		if c, err := New(context.Background(), endpoint, "token", root); err == nil {
			c.Close()
			t.Fatalf("accepted endpoint %s", endpoint)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "untracked"), []byte("user data"), 0644); err != nil {
		t.Fatal(err)
	}
	if c, err := New(context.Background(), "http://127.0.0.1:8797", "token", root); err == nil {
		c.Close()
		t.Fatal("accepted dirty workspace")
	}
}

func TestBridgeProcess(t *testing.T) {
	if os.Getenv("CODEAGENT_P0A_TEST_HELPER") != "1" {
		t.Skip("stdio subprocess helper")
	}
	c, err := New(context.Background(), os.Getenv("P0A_TEST_ENDPOINT"), os.Getenv("P0A_TEST_TOKEN"), os.Getenv("P0A_TEST_WORKSPACE"))
	if err != nil {
		os.Exit(2)
	}
	err = Server(c).Run(context.Background(), &mcp.StdioTransport{})
	c.Close()
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0) // The Go test runner must not write PASS to MCP stdout.
}
