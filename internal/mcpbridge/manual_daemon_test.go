//go:build unix

package mcpbridge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code-agent/internal/executionevidence"
	"code-agent/internal/mcpbridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The new profile must work with real Session/SQLite/TurnExecutor components,
// not just a fake routing endpoint. Only the provider responses are scripted.
func TestManualRealDaemonAndSQLiteHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	source, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "clone", "--quiet", "--shared", "--", strings.TrimSpace(string(source)), root).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s %v", out, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	var steps atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool
			Tools  []json.RawMessage
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad fixture request", 400)
			return
		}
		message := map[string]any{"content": "local scripted execution finished"}
		finish := "stop"
		if len(req.Tools) > 0 && steps.Add(1) == 1 {
			message = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "manual-command", "type": "function", "function": map[string]string{"name": "run_command", "arguments": `{"command":"go version"}`}}}}
			finish = "tool_calls"
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			body, _ := json.Marshal(map[string]any{"model": "manual-scripted", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "manual-scripted", "choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
		}
	}))
	defer provider.Close()
	config, _ := json.Marshal(map[string]any{"default_model": "scripted", "providers": map[string]any{"local": map[string]any{"api": "openai", "base_url": provider.URL, "models": []any{map[string]any{"id": "scripted", "runtime_alias": "scripted", "context_window": 32000, "supports_tools": true}}}}, "credentials": map[string]any{"llm": map[string]any{"local": map[string]string{"source": "none"}}}, "agent": map[string]int{"max_steps": 4}, "approval_mode": "auto"})
	const token = "manual-local-fixture-0123456789abcdef"
	endpoint := startExperimentRuntime(t, ctx, root, config, token)
	identity := func(path string) map[string]any {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", endpoint+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("identity status: %d", res.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if data, ok := out["data"].(map[string]any); ok {
			return data
		}
		return out
	}
	runtimeInfo, storeInfo := identity("/v1/runtime/info"), identity("/v1/tasks/capabilities")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "connection.json")
	grant, _ := json.Marshal(map[string]any{"schema_version": 1, "endpoint": endpoint, "server_id": runtimeInfo["server_id"], "store_id": storeInfo["store_id"], "instance_id": "manual-daemon-test", "daemon_pid": os.Getpid(), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "access_token": token})
	if err := os.WriteFile(path, grant, 0600); err != nil {
		t.Fatal(err)
	}
	service, err := mcpbridge.NewManualService(ctx, path, root)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	a, b := mcp.NewInMemoryTransports()
	ss, err := mcpbridge.ManualServer(service).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "manual-real-daemon"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call := func(name string, in, out any) {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("%s: %+v %v", name, result, err)
		}
		data, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	var info mcpbridge.ManualConnectionInfo
	call("codeagent_connection_info", map[string]any{}, &info)
	var ref mcpbridge.ManualBindingRef
	call("codeagent_create_session", mcpbridge.ManualCreateInput{ManualIdentity: mcpbridge.ManualIdentity{ServerID: info.ServerID, StoreID: info.StoreID, Baseline: info.HeadCommit}, RequestID: "manual-create"}, &ref)
	var receipt mcpbridge.Receipt
	call("codeagent_send_to_session", map[string]any{"binding_id": ref.BindingID, "session_id": ref.ID, "request_id": "manual-turn", "message": "Run the fixed local scripted tool fixture only.", "development": executionevidence.Expectations{ValidationCommands: []string{"go version"}}}, &receipt)
	var update mcpbridge.Update
	for !update.Terminal {
		call("codeagent_wait_for_update", map[string]any{"binding_id": ref.BindingID, "session_id": ref.ID, "turn_id": receipt.TurnID, "after_cursor": update.Cursor, "timeout_seconds": 20}, &update)
	}
	var handoff mcpbridge.Handoff
	call("codeagent_get_handoff", map[string]any{"binding_id": ref.BindingID, "session_id": ref.ID, "turn_id": receipt.TurnID}, &handoff)
	if handoff.ExecutionStatus != "completed" || handoff.ValidationStatus != "complete" || handoff.AcceptanceStatus != "not_reviewed" {
		t.Fatalf("handoff: %+v", handoff)
	}
	if len(handoff.Commands) != 1 || handoff.Commands[0].ExitCode == nil || *handoff.Commands[0].ExitCode != 0 || handoff.Commands[0].Evidence.TurnID != receipt.TurnID {
		t.Fatalf("command evidence: %+v", handoff.Commands)
	}
	if handoff.ValidationSource != "persisted_turn_accepted" || handoff.ValidationContractRef == nil || handoff.ValidationContractRef.AcceptedSeq != receipt.Cursor || handoff.ValidationContractRef.RequestID != "manual-turn" || handoff.ValidationContractRef.StoreID != ref.StoreID {
		t.Fatalf("missing durable contract provenance: %+v", handoff)
	}
	before := steps.Load()
	// Destroy the Bridge and its MCP session; neither expectations nor receipt
	// caches survive. Bind by explicit original identity, then read once.
	_ = client.Close()
	_ = ss.Close()
	service.Close()
	replacement, err := mcpbridge.NewManualService(ctx, path, root)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	a2, b2 := mcp.NewInMemoryTransports()
	ss2, err := mcpbridge.ManualServer(replacement).Connect(ctx, a2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss2.Close()
	client, err = mcp.NewClient(&mcp.Implementation{Name: "manual-restarted-bridge"}, nil).Connect(ctx, b2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var rebound mcpbridge.ManualBindingRef
	call("codeagent_bind_session", mcpbridge.ManualBindInput{ManualIdentity: ref.ManualIdentity, SessionID: ref.ID}, &rebound)
	var recovered mcpbridge.Handoff
	call("codeagent_get_handoff", map[string]any{"binding_id": rebound.BindingID, "session_id": ref.ID, "turn_id": receipt.TurnID}, &recovered)
	if recovered.ValidationStatus != "complete" || recovered.ValidationSource != "persisted_turn_accepted" || recovered.ValidationContractHash != handoff.ValidationContractHash || recovered.ValidationContractRef == nil || *recovered.ValidationContractRef != *handoff.ValidationContractRef || recovered.AcceptanceStatus != "not_reviewed" || recovered.DiffSHA256 != handoff.DiffSHA256 || steps.Load() != before {
		t.Fatalf("restart lost exact persisted evidence or reran execution: %+v", recovered)
	}

}
