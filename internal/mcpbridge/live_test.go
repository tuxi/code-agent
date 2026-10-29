package mcpbridge_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/executionevidence"
	"code-agent/internal/mcpbridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestLiveDaemonRoundTrip is opt-in and consumes the explicitly configured
// model's API quota. Normal tests never contact a model. The caller supplies a
// disposable clean repository, a built bridge binary and a reviewed task file.
func TestLiveDaemonRoundTrip(t *testing.T) {
	if os.Getenv("CODEAGENT_P0A_LIVE") != "1" {
		t.Skip("set CODEAGENT_P0A_LIVE=1 for the real-model experiment")
	}
	root, binary, promptFile, output := os.Getenv("CODEAGENT_P0A_WORKSPACE"), os.Getenv("CODEAGENT_P0A_BRIDGE"), os.Getenv("CODEAGENT_P0A_PROMPT"), os.Getenv("CODEAGENT_P0A_OUTPUT")
	if !filepath.IsAbs(root) || !filepath.IsAbs(binary) || !filepath.IsAbs(promptFile) || !filepath.IsAbs(output) {
		t.Fatal("explicit absolute workspace, bridge, prompt and output paths are required")
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	var expected executionevidence.Expectations
	if err = json.Unmarshal([]byte(os.Getenv("CODEAGENT_P0A_VALIDATION_COMMANDS")), &expected.ValidationCommands); err != nil || len(expected.ValidationCommands) == 0 {
		t.Fatal("CODEAGENT_P0A_VALIDATION_COMMANDS must explicitly declare a nonempty JSON array of expected foreground checks")
	}

	model := os.Getenv("CODEAGENT_P0A_MODEL") // empty inherits the supplied runtime default
	configPath := os.Getenv("CODEAGENT_P0A_SETTINGS")
	if !filepath.IsAbs(configPath) {
		t.Fatal("CODEAGENT_P0A_SETTINGS must explicitly name isolated runtime settings")
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("cannot read isolated runtime settings")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret)
	// Real production components, isolated data and no global MCP discovery.
	// The user's running app and configuration are not reconfigured.
	endpoint := startExperimentRuntime(t, ctx, root, config, token)
	cmd := exec.Command(binary, "--experimental-p0a", "--runtime-url", endpoint, "--workspace", root)
	cmd.Env = append(os.Environ(), "CODEAGENT_BRIDGE_TOKEN="+token)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "codex-p0a-experiment-driver", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call := func(name string, in, out any) {
		t.Helper()
		res, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("%s: %v", name, res.Content)
		}
		data, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, value any) {
		t.Helper()
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(output, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	var catalog mcpbridge.ModelCatalog
	call("codeagent_list_models", struct{}{}, &catalog)
	write("catalog.json", catalog)
	var ref mcpbridge.SessionRef
	call("codeagent_create_session", mcpbridge.CreateInput{RequestID: "p0a-create"}, &ref)
	var receipt mcpbridge.Receipt
	call("codeagent_send_to_session", mcpbridge.SendInput{SessionID: ref.ID, RequestID: "p0a-implement-1", Message: string(prompt), Model: model, Development: &expected}, &receipt)
	write("receipt.json", receipt)
	result, waitErr := mcpbridge.AwaitCompletion(ctx, func(context.Context) (mcpbridge.ReadResult, error) {
		var snapshot mcpbridge.ReadResult
		call("codeagent_wait_sessions", mcpbridge.WaitInput{SessionID: ref.ID, TurnID: receipt.TurnID, Cursor: receipt.Cursor, TimeoutSeconds: 30}, &snapshot)
		return snapshot, nil
	}, func(snapshot mcpbridge.ReadResult) { write("state.json", snapshot) })
	write("run.json", map[string]any{"model": model, "elapsed_ms": time.Since(started).Milliseconds(), "status": result.Status, "workspace": root,
		"transport": "stdio MCP -> HTTP/WebSocket -> actual CodeAgent runtime", "planner_reviewer": "Codex in the parent chat; experiment driver performs prescribed MCP calls, not autonomous Codex CLI tool selection"})
	if waitErr != nil {
		write("attention-or-stop.json", result)
		t.Fatalf("%v; no final artifacts collected; inspect %s", waitErr, output)
	}
	call("codeagent_read_session", mcpbridge.ReadInput{SessionID: ref.ID, TurnID: receipt.TurnID}, &result)
	write("events.json", result)
	if result.Status != "completed" {
		t.Fatalf("real task stopped in %s; inspect %s", result.Status, output)
	}
	var artifacts mcpbridge.Artifacts
	call("codeagent_get_artifacts", mcpbridge.ArtifactInput{SessionID: ref.ID, TurnID: receipt.TurnID}, &artifacts)
	write("artifacts.json", artifacts)
	if artifacts.ExecutionEvidence == nil || artifacts.ExecutionEvidence.Status != "evidence_complete" {
		t.Fatalf("needs_validation; turn completion is not evidence completion; inspect %s", output)
	}
	if artifacts.Truncated || len(artifacts.CommandResults) == 0 || strings.TrimSpace(artifacts.Diff) == "" {
		t.Fatalf("incomplete evidence; inspect %s", output)
	}
	t.Logf("evidence=%s commands=%d elapsed=%s", output, len(artifacts.CommandResults), time.Since(started).Round(time.Second))
}
