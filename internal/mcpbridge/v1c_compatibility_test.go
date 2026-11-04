//go:build unix

package mcpbridge_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/mcpbridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type compatRuntime struct {
	endpoint, db, token, grant string
	pid                        int
	stop                       func()
}

func startCompatRuntime(t *testing.T, ctx context.Context, binary, db string) compatRuntime {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret[:])
	tokenPath, ready := filepath.Join(dir, "token"), filepath.Join(dir, "ready.json")
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "fixture.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, "--db", db, "--ready", ready, "--token-file", tokenPath)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); log.Close() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		})
	}
	t.Cleanup(stop)
	var info struct {
		Endpoint string `json:"endpoint"`
	}
	for info.Endpoint == "" {
		if data, err := os.ReadFile(ready); err == nil {
			_ = json.Unmarshal(data, &info)
		}
		if info.Endpoint != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture startup timed out")
		case <-time.After(10 * time.Millisecond):
		}
	}
	r := compatRuntime{endpoint: info.Endpoint, db: db, token: token, pid: cmd.Process.Pid, grant: filepath.Join(dir, "grant.json"), stop: stop}
	var identity struct {
		ServerID string `json:"server_id"`
		StoreID  string `json:"store_id"`
	}
	r.get(t, ctx, "/v1/tasks/capabilities", &identity)
	grant, _ := json.Marshal(map[string]any{"schema_version": 1, "endpoint": r.endpoint, "server_id": identity.ServerID, "store_id": identity.StoreID, "instance_id": "compat-fixture", "daemon_pid": r.pid, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "access_token": token})
	if err := os.WriteFile(r.grant, grant, 0600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r compatRuntime) get(t *testing.T, ctx context.Context, path string, out any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.endpoint+path, nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	response, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("fixture read %s: %d", path, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Code != nil {
		if *envelope.Code != 0 {
			t.Fatal("fixture API error")
		}
		raw = envelope.Data
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func compatMCP(t *testing.T, ctx context.Context, binary string, runtime compatRuntime, root string, manual bool) *mcp.ClientSession {
	t.Helper()
	args := []string{"--experimental-p0a", "--connection-manifest", runtime.grant, "--workspace", root}
	if manual {
		args = append(args, "--profile", "manual")
	}
	cmd := exec.Command(binary, args...)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "v1c-compatibility"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func compatCall(t *testing.T, ctx context.Context, client *mcp.ClientSession, name string, in, out any) {
	t.Helper()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("%s failed: %v %+v", name, err, result)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func compatRepo(t *testing.T) string {
	t.Helper()
	source, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "clone", "--quiet", "--shared", strings.TrimSpace(string(source)), root).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s %v", out, err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// Capture schema and original immutable input facts without mutating the DB.
func compatStoreFacts(t *testing.T, path, id, request string) (string, string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_master ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	var schema [][4]string
	for rows.Next() {
		var row [4]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			t.Fatal(err)
		}
		schema = append(schema, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var hash, payload, state string
	var seq int64
	if err := db.QueryRow(`SELECT payload_hash,accepted_seq,state FROM turn_inputs WHERE session_id=? AND request_id=?`, id, request).Scan(&hash, &seq, &state); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT payload FROM session_events WHERE id=?`, seq).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(schema)
	b, _ := json.Marshal([]any{hash, seq, state, payload})
	sa, sb := sha256.Sum256(a), sha256.Sum256(b)
	return hex.EncodeToString(sa[:]), hex.EncodeToString(sb[:])
}

func TestV1CCrossVersionProcesses(t *testing.T) {
	oldRuntime, newRuntime := os.Getenv("CODEAGENT_COMPAT_OLD_RUNTIME"), os.Getenv("CODEAGENT_COMPAT_NEW_RUNTIME")
	oldBridge, newBridge := os.Getenv("CODEAGENT_COMPAT_OLD_BRIDGE"), os.Getenv("CODEAGENT_COMPAT_NEW_BRIDGE")
	for _, path := range []string{oldRuntime, newRuntime, oldBridge, newBridge} {
		if path == "" {
			t.Skip("explicit locally built baseline/candidate fixture and Bridge binaries required")
		}
		if !filepath.IsAbs(path) {
			t.Fatal("absolute fixture binary required")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	t.Run("new_manual_old_daemon_then_upgrade", func(t *testing.T) {
		root := compatRepo(t)
		db := filepath.Join(t.TempDir(), "store.db")
		old := startCompatRuntime(t, ctx, oldRuntime, db)
		client := compatMCP(t, ctx, newBridge, old, root, true)
		var info mcpbridge.ManualConnectionInfo
		compatCall(t, ctx, client, "codeagent_connection_info", map[string]any{}, &info)
		var ref mcpbridge.ManualBindingRef
		compatCall(t, ctx, client, "codeagent_create_session", mcpbridge.ManualCreateInput{ManualIdentity: mcpbridge.ManualIdentity{ServerID: info.ServerID, StoreID: info.StoreID, Baseline: info.HeadCommit}, RequestID: "create"}, &ref)
		input := map[string]any{"binding_id": ref.BindingID, "session_id": ref.ID, "request_id": "legacy", "message": "offline task"}
		withContract := map[string]any{"binding_id": ref.BindingID, "session_id": ref.ID, "request_id": "rejected", "message": "offline task", "development": map[string]any{"validation_commands": []string{"go version"}}}
		rejected, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_send_to_session", Arguments: withContract})
		if err != nil || rejected == nil || !rejected.IsError {
			t.Fatal("old daemon accepted a contract")
		}
		raw, _ := json.Marshal(rejected.Content)
		if !strings.Contains(string(raw), "validation_contract_unsupported") {
			t.Fatal("missing structured rejection")
		}
		var count struct {
			Runs int `json:"runs"`
		}
		old.get(t, ctx, "/compat/count", &count)
		if count.Runs != 0 {
			t.Fatal("rejection dispatched work")
		}
		if tools, err := client.ListTools(ctx, nil); err != nil || len(tools.Tools) != 11 {
			t.Fatal("tool error broke MCP")
		}
		var receipt mcpbridge.Receipt
		compatCall(t, ctx, client, "codeagent_send_to_session", input, &receipt)
		var h mcpbridge.Handoff
		compatCall(t, ctx, client, "codeagent_get_handoff", map[string]any{"binding_id": ref.BindingID, "session_id": ref.ID, "turn_id": receipt.TurnID}, &h)
		if h.ExecutionStatus != "completed" || h.ValidationStatus != "unknown" || h.ValidationSource != "not_declared" {
			t.Fatal(h)
		}
		_ = client.Close()
		old.stop()
		schema, original := compatStoreFacts(t, db, ref.ID, "legacy")
		current := startCompatRuntime(t, ctx, newRuntime, db)
		client = compatMCP(t, ctx, newBridge, current, root, true)
		var bound mcpbridge.ManualBindingRef
		compatCall(t, ctx, client, "codeagent_bind_session", mcpbridge.ManualBindInput{ManualIdentity: ref.ManualIdentity, SessionID: ref.ID}, &bound)
		compatCall(t, ctx, client, "codeagent_get_handoff", map[string]any{"binding_id": bound.BindingID, "session_id": ref.ID, "turn_id": receipt.TurnID}, &h)
		if h.ValidationStatus != "unknown" || h.ValidationSource != "not_declared" || h.ValidationContractRef != nil {
			t.Fatal("upgrade fabricated contract", h)
		}
		if after, fact := compatStoreFacts(t, db, ref.ID, "legacy"); after != schema || fact != original {
			t.Fatal("upgrade changed schema or original acceptance")
		}
		withContract["binding_id"], withContract["request_id"] = bound.BindingID, "new-contract"
		compatCall(t, ctx, client, "codeagent_send_to_session", withContract, &receipt)
		compatCall(t, ctx, client, "codeagent_get_handoff", map[string]any{"binding_id": bound.BindingID, "session_id": ref.ID, "turn_id": receipt.TurnID}, &h)
		if h.ValidationSource != "persisted_turn_accepted" || h.ValidationStatus != "partial" || h.AcceptanceStatus != "not_reviewed" || h.ValidationContractRef == nil {
			t.Fatal(h)
		}
		originalHash := h.ValidationContractHash
		_ = client.Close()
		client = compatMCP(t, ctx, newBridge, current, root, true)
		compatCall(t, ctx, client, "codeagent_bind_session", mcpbridge.ManualBindInput{ManualIdentity: ref.ManualIdentity, SessionID: ref.ID}, &bound)
		compatCall(t, ctx, client, "codeagent_get_handoff", map[string]any{"binding_id": bound.BindingID, "session_id": ref.ID, "turn_id": receipt.TurnID}, &h)
		current.get(t, ctx, "/compat/count", &count)
		if count.Runs != 1 || h.ValidationContractHash != originalHash || h.ValidationStatus != "partial" {
			t.Fatal("Bridge restart lost contract or replayed execution", count.Runs, h)
		}
		t.Log("old daemon contract rejection: 0 executions; old Store schema/acceptance unchanged on upgrade; fresh MCP process recovered original contract with no replay; Provider calls=0")
	})
	t.Run("old_bridge_new_daemon", func(t *testing.T) {
		root := compatRepo(t)
		current := startCompatRuntime(t, ctx, newRuntime, filepath.Join(t.TempDir(), "store.db"))
		client := compatMCP(t, ctx, oldBridge, current, root, false)
		var ref mcpbridge.SessionRef
		compatCall(t, ctx, client, "codeagent_create_session", map[string]any{"request_id": "old-create"}, &ref)
		var receipt mcpbridge.Receipt
		input := map[string]any{"session_id": ref.ID, "request_id": "old-request", "message": "offline task", "development": map[string]any{"validation_commands": []string{"go version"}}}
		compatCall(t, ctx, client, "codeagent_send_to_session", input, &receipt)
		var duplicate mcpbridge.Receipt
		compatCall(t, ctx, client, "codeagent_send_to_session", input, &duplicate)
		if receipt != duplicate {
			t.Fatal("legacy idempotent receipt changed")
		}
		var h mcpbridge.Handoff
		compatCall(t, ctx, client, "codeagent_get_handoff", map[string]any{"session_id": ref.ID, "turn_id": receipt.TurnID}, &h)
		if h.ExecutionStatus != "completed" || h.ValidationStatus != "partial" || h.AcceptanceStatus != "not_reviewed" {
			t.Fatal(h)
		}
		var count struct {
			Runs int `json:"runs"`
		}
		current.get(t, ctx, "/compat/count", &count)
		if count.Runs != 1 {
			t.Fatal("legacy retry duplicated execution", count.Runs)
		}
		t.Log("actual baseline Bridge -> candidate Daemon: legacy tools/receipt/Handoff compatible; duplicate request executions=1; Provider calls=0")
	})
}
