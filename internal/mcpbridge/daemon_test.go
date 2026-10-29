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

	"code-agent/internal/app"
	"code-agent/internal/conversation"
	"code-agent/internal/executionevidence"
	"code-agent/internal/mcpbridge"
	"code-agent/internal/runtime"
	"code-agent/internal/server"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This verifies the real runtime/tool loop and stdio MCP boundary without an
// external model. The scripted provider is explicitly not an agent-quality test.
func TestDaemonStdioToolRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rootOut, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "clone", "--quiet", "--shared", "--", strings.TrimSpace(string(rootOut)), root).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s %v", out, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	var step atomic.Int32
	original, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	editArgs, _ := json.Marshal(map[string]string{"path": "README.md", "old": string(original), "new": "P0-A local scripted tool execution\n"})
	badEditArgs, _ := json.Marshal(map[string]string{"old": string(original), "new": "P0-A local scripted tool execution\n"})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool              `json:"stream"`
			Tools  []json.RawMessage `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		message := map[string]any{"content": "Local scripted fixture completed."}
		finish := "stop"
		if len(req.Tools) > 0 {
			var name, args string
			switch step.Add(1) {
			case 1:
				name, args = "read_file", `{"path":"README.md"}`
			case 2:
				// Reproduce the real experiment's missing-path failure. The next
				// scripted response repairs it without any human approval frame.
				name, args = "edit_file", string(badEditArgs)
			case 3, 4:
				// Runtime's existing hypothesis reminder drops the first mutation
				// after a tool failure. Reissue the same corrected call next.
				name, args = "edit_file", string(editArgs)
			case 5:
				name, args = "run_command", `{"command":"go version"}`
			}
			if name != "" {
				finish = "tool_calls"
				message = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("fixture-%d", step.Load()), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}
			}
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			data, _ := json.Marshal(map[string]any{"model": "served-scripted", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": nil}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			data, _ = json.Marshal(map[string]any{"model": "served-scripted", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "served-scripted", "choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
		}
	}))
	defer provider.Close()
	config, _ := json.Marshal(map[string]any{
		"default_model": "scripted", "providers": map[string]any{"local": map[string]any{"api": "openai", "base_url": provider.URL, "models": []any{map[string]any{"id": "scripted", "runtime_alias": "scripted", "context_window": 32000, "supports_tools": true}}}},
		"credentials": map[string]any{"llm": map[string]any{"local": map[string]string{"source": "none"}}},
		"agent":       map[string]int{"max_steps": 8}, "approval_mode": "auto",
	})
	const token = "local-fixture-token-0123456789abcdef"
	endpoint := startExperimentRuntime(t, ctx, root, config, token)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBridgeProcess$")
	cmd.Env = append(os.Environ(), "CODEAGENT_P0A_TEST_HELPER=1", "P0A_TEST_ENDPOINT="+endpoint, "P0A_TEST_TOKEN="+token, "P0A_TEST_WORKSPACE="+root)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "local-protocol-test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call := func(name string, in, out any) {
		t.Helper()
		r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
		if err != nil {
			t.Fatal(err)
		}
		if r.IsError {
			data, _ := json.Marshal(r.Content)
			t.Fatalf("%s: %s", name, data)
		}
		data, _ := json.Marshal(r.StructuredContent)
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	var catalog mcpbridge.ModelCatalog
	call("codeagent_list_models", struct{}{}, &catalog)
	if catalog.DefaultModel != "local/scripted" || len(catalog.Models) != 1 || !catalog.Models[0].Available {
		t.Fatalf("catalog=%+v", catalog)
	}
	var ref mcpbridge.SessionRef
	call("codeagent_create_session", mcpbridge.CreateInput{RequestID: "local-create"}, &ref)
	var receipt mcpbridge.Receipt
	call("codeagent_send_to_session", mcpbridge.SendInput{SessionID: ref.ID, RequestID: "local-turn", Message: "Execute the prescribed local fixture operations. Do not delegate or commit. This is a transport test.",
		Development: &executionevidence.Expectations{ValidationCommands: []string{"go version"}}}, &receipt)
	var state mcpbridge.ReadResult
	call("codeagent_wait_sessions", mcpbridge.WaitInput{SessionID: ref.ID, TurnID: receipt.TurnID, Cursor: receipt.Cursor, TimeoutSeconds: 20}, &state)
	if state.Status != "completed" || !state.Terminal || state.AttentionRequired {
		t.Fatalf("runtime did not complete: %+v", state)
	}
	var invalidCalls int
	for _, event := range state.Events {
		if event.Kind == "tool_finished" {
			data, _ := json.Marshal(event.Output)
			if strings.Contains(string(data), `"code":"invalid_tool_arguments"`) && strings.Contains(string(data), `"field":"path"`) {
				invalidCalls++
			}
		}
	}
	if invalidCalls != 1 {
		t.Fatalf("missing durable structured argument error: %d", invalidCalls)
	}
	var artifacts mcpbridge.Artifacts
	call("codeagent_get_artifacts", mcpbridge.ArtifactInput{SessionID: ref.ID, TurnID: receipt.TurnID}, &artifacts)
	if !strings.Contains(artifacts.Diff, "P0-A local scripted tool execution") || artifacts.Truncated {
		t.Fatalf("real tool diff missing: %+v", artifacts)
	}
	if len(artifacts.CommandResults) != 1 || artifacts.CommandResults[0].ExitCode == nil || *artifacts.CommandResults[0].ExitCode != 0 || !strings.Contains(artifacts.CommandResults[0].Stdout, "go version") {
		t.Fatalf("actual command evidence missing: %+v", artifacts)
	}
	if len(artifacts.ModelCalls) != 6 || artifacts.TaskAcceptance != "not_reviewed" {
		t.Fatalf("model evidence=%+v", artifacts)
	}
	if artifacts.ExecutionEvidence == nil || artifacts.ExecutionEvidence.Status != "evidence_complete" {
		t.Fatalf("execution evidence incomplete: %+v", artifacts.ExecutionEvidence)
	}
	for _, call := range artifacts.ModelCalls {
		if call.Selection == nil || call.Selection.RequestedModel != nil || call.Selection.ResolvedModel != "local/scripted" || call.Selection.ActualModel != "served-scripted" || call.PromptTokens != 10 || call.Succeeded == nil || !*call.Succeeded {
			t.Fatalf("model call=%+v", call)
		}
	}
	call("codeagent_list_models", struct{}{}, &catalog)
	if !catalog.Models[0].Verified || catalog.Models[0].VerifiedAt == "" {
		t.Fatalf("missing actual verification=%+v", catalog)
	}
	// Explicit selection is a new Turn and must not alter the runtime default.
	call("codeagent_send_to_session", mcpbridge.SendInput{SessionID: ref.ID, RequestID: "selected-turn", Message: "Report completion.", Model: "local/scripted"}, &receipt)
	call("codeagent_wait_sessions", mcpbridge.WaitInput{SessionID: ref.ID, TurnID: receipt.TurnID, Cursor: receipt.Cursor, TimeoutSeconds: 10}, &state)
	call("codeagent_get_artifacts", mcpbridge.ArtifactInput{SessionID: ref.ID, TurnID: receipt.TurnID}, &artifacts)
	if len(artifacts.ModelCalls) != 1 || artifacts.ModelCalls[0].Selection.RequestedModel == nil || *artifacts.ModelCalls[0].Selection.RequestedModel != "local/scripted" {
		t.Fatalf("explicit selection evidence=%+v", artifacts.ModelCalls)
	}
}

func startExperimentRuntime(t *testing.T, ctx context.Context, root string, config []byte, token string) string {
	t.Helper()
	// Assemble the real production components with MCP discovery deliberately
	// disabled. A test must not start the user's configured global MCP servers.
	sf, err := settings.ParseJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := app.FromSettings(sf.ToSettings())
	if err != nil {
		t.Fatal(err)
	}
	cfg.GlobalSkillsDir = t.TempDir()
	mc, err := app.SelectModel("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	cred := cfg.CredentialResolver(nil)
	model, err := runtime.BuildProvider(mc, cfg.Provider, cred)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	previousFactory := runtime.StoreFactory
	runtime.StoreFactory = func(string) (session.Store, error) { return sqlite.New(dbPath) }
	t.Cleanup(func() { runtime.StoreFactory = previousFactory })
	registry, _, plan, _, err := runtime.BuildBaseRegistry(ctx, cfg, mc, model, cred, store, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	workspaces := runtime.NewWorkspaceRegistry(cfg.GlobalSkillsDir)
	t.Cleanup(func() { _ = workspaces.Close() })
	repo := conversation.NewSQLiteRepository(store, mc.ContextWindow, 24000, mc.Model, "", nil)
	events := &conversation.StoreEventAdapter{Store: store}
	builder := runtime.NewServeRunBuilder(cfg, mc, model, cred, registry, workspaces, plan)
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), builder)
	t.Cleanup(executor.Shutdown)
	daemon := httptest.NewServer(server.WithServerAuth(server.NewMux(repo, events, executor, server.MuxOptions{RuntimeModelsBuilder: func() server.RuntimeModelCatalog { return server.BuildRuntimeModelCatalog(cfg, cred) }}), server.ServerAuth{Enabled: true, Token: token}))
	t.Cleanup(daemon.Close)
	return daemon.URL
}
