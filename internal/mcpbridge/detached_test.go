//go:build unix

package mcpbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code-agent/internal/executionevidence"
	"code-agent/internal/mcpbridge"
	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"
)

// Only the test runner launches this helper. It exercises the production MCP
// server, without adding a new desktop launcher or a Runtime implementation.
func TestDetachedBridgeProcess(t *testing.T) {
	if os.Getenv("CODEAGENT_DETACHED_HELPER") != "1" {
		t.Skip("stdio subprocess helper")
	}
	ctx := context.Background()
	var c *mcpbridge.Client
	var err error
	if id := os.Getenv("DETACHED_SESSION"); id != "" {
		c, _, err = mcpbridge.NewReattachedFromManifest(ctx, os.Getenv("DETACHED_MANIFEST"), os.Getenv("DETACHED_WORKSPACE"), id, os.Getenv("DETACHED_BASELINE"))
	} else {
		c, _, err = mcpbridge.NewFromManifest(ctx, os.Getenv("DETACHED_MANIFEST"), os.Getenv("DETACHED_WORKSPACE"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	err = mcpbridge.Server(c).Run(ctx, &mcp.StdioTransport{})
	c.Close()
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestDaemonContinuesAfterBridgeExitWithoutClientPolling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	source, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "clone", "--quiet", "--shared", "--", strings.TrimSpace(string(source)), root).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseline := strings.TrimSpace(string(head))
	release := make(chan struct{})
	entered := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var step atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool              `json:"stream"`
			Tools  []json.RawMessage `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		var message any = map[string]string{"content": "detached fixture done"}
		finish := "stop"
		if len(req.Tools) > 0 {
			n := step.Add(1)
			if n == 1 {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			name, args := "", ""
			if n == 1 {
				name, args = "read_file", `{"path":"README.md"}`
			}
			if n == 2 {
				name, args = "run_command", `{"command":"go version"}`
			}
			if name != "" {
				finish = "tool_calls"
				message = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("detached-%d", n), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}
			}
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			body, _ := json.Marshal(map[string]any{"model": "local-scripted", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"model": "local-scripted", "choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
		}
	}))
	defer func() { releaseOnce.Do(func() { close(release) }); provider.Close() }()
	config, _ := json.Marshal(map[string]any{"default_model": "scripted", "providers": map[string]any{"local": map[string]any{"api": "openai", "base_url": provider.URL, "models": []any{map[string]any{"id": "scripted", "runtime_alias": "scripted", "context_window": 32000, "supports_tools": true}}}}, "credentials": map[string]any{"llm": map[string]any{"local": map[string]string{"source": "none"}}}, "agent": map[string]int{"max_steps": 4}, "approval_mode": "auto"})
	const token = "detached-fixture-token-0123456789abcdef"
	var detached atomic.Bool
	var polls atomic.Int32
	endpoint := startObservedExperimentRuntime(t, ctx, root, config, token, func(r *http.Request) {
		if detached.Load() && (strings.HasSuffix(r.URL.Path, "/events") || r.URL.Path == "/v1/activity") {
			polls.Add(1)
		}
	})
	identity := func(path string) map[string]any {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", endpoint+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("test fixture identity %s: HTTP %d", path, res.StatusCode)
		}
		var out map[string]any
		if err = json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if data, ok := out["data"].(map[string]any); ok {
			out = data
		}
		return out
	}
	runtimeID, storeID := identity("/v1/runtime/info"), identity("/v1/tasks/capabilities")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "connection.json")
	grant, _ := json.Marshal(map[string]any{"schema_version": 1, "endpoint": endpoint, "server_id": runtimeID["server_id"], "store_id": storeID["store_id"], "instance_id": "detached-test", "daemon_pid": os.Getpid(), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "access_token": token})
	if err = os.WriteFile(manifest, grant, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var firstProcess *os.Process
	connect := func(id string) *mcp.ClientSession {
		t.Helper()
		cmd := exec.Command(exe, "-test.run=^TestDetachedBridgeProcess$")
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "CODEAGENT_DETACHED_HELPER=1", "DETACHED_MANIFEST="+manifest, "DETACHED_WORKSPACE="+root, "DETACHED_BASELINE="+baseline, "DETACHED_SESSION="+id)
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "detached-test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			firstProcess = cmd.Process
		}
		return cs
	}
	client := connect("")
	call := func(cs *mcp.ClientSession, name string, in, out any) {
		t.Helper()
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s: %v", name, result.Content)
		}
		b, _ := json.Marshal(result.StructuredContent)
		if err = json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}
	var ref mcpbridge.SessionRef
	call(client, "codeagent_create_session", mcpbridge.CreateInput{RequestID: "create-detached"}, &ref)
	// This push-only observer stands in for Talkify. It never sends inputs or
	// polls status and is not claimed as an actual desktop GUI verification.
	observer, _, err := websocket.Dial(ctx, strings.Replace(endpoint, "http://", "ws://", 1)+"/v1/conversations/"+ref.ID+"/stream", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.CloseNow()
	if _, _, err = observer.Read(ctx); err != nil {
		t.Fatal(err)
	}
	var receipt mcpbridge.Receipt
	call(client, "codeagent_send_to_session", mcpbridge.SendInput{SessionID: ref.ID, RequestID: "detached-turn", Message: "Read README.md, run go version and finish. Local scripted fixture only.", Development: &executionevidence.Expectations{ValidationCommands: []string{"go version"}}}, &receipt)
	if !receipt.Accepted {
		t.Fatal("no durable receipt")
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	// Also prove the process is gone before releasing the fixture, rather than
	// treating a disconnected SDK session as evidence of process termination.
	if err = firstProcess.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	exitDeadline := time.Now().Add(2 * time.Second)
	for unix.Kill(firstProcess.Pid, 0) != unix.ESRCH {
		if time.Now().After(exitDeadline) {
			t.Fatal("Bridge process still present before execution")
		}
		time.Sleep(5 * time.Millisecond)
	}
	detached.Store(true)
	releaseOnce.Do(func() { close(release) })
	for {
		_, data, err := observer.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var event mcpbridge.Event
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		if event.TurnID == receipt.TurnID && (event.Kind == "turn_failed" || event.Kind == "turn_cancelled") {
			t.Fatalf("execution stopped on disconnect: %+v", event)
		}
		if event.TurnID == receipt.TurnID && event.Kind == "turn_finished" {
			break
		}
	}
	if polls.Load() != 0 {
		t.Fatalf("detached runtime polls=%d", polls.Load())
	}
	detached.Store(false)
	resumed := connect(ref.ID)
	defer resumed.Close()
	var handoff mcpbridge.Handoff
	// Exactly one review-time tool call. No read/wait/lookup is needed here.
	call(resumed, "codeagent_get_handoff", mcpbridge.HandoffInput{SessionID: ref.ID, TurnID: receipt.TurnID}, &handoff)
	if handoff.ExecutionStatus != "completed" || handoff.AcceptanceStatus != "not_reviewed" || handoff.ValidationStatus != "unknown" || len(handoff.Commands) != 1 || handoff.Commands[0].ExitCode == nil || *handoff.Commands[0].ExitCode != 0 {
		t.Fatalf("%+v", handoff)
	}
	t.Logf("send=1; client/Bridge disconnected; execution-time HTTP polls=%d; review tool calls=1; execution=%s validation=%s acceptance=%s", polls.Load(), handoff.ExecutionStatus, handoff.ValidationStatus, handoff.AcceptanceStatus)
}
