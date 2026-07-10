package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/app"
	"code-agent/internal/tools"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpServerEnv, when set, flips the test binary into a minimal MCP stdio server
// exposing exactly one tool named by the variable's value. Workspace .mcp.json
// files written by the tests point their `command` at the test binary itself
// (os.Executable), so the isolation tests exercise the REAL connect path — a
// spawned subprocess speaking MCP over stdio — without any external fixture.
const mcpServerEnv = "CODEAGENT_TEST_MCP_TOOL"

func TestMain(m *testing.M) {
	if toolName := os.Getenv(mcpServerEnv); toolName != "" {
		runTestMCPServer(toolName)
		return
	}
	os.Exit(m.Run())
}

func runTestMCPServer(toolName string) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "codeagent-test", Version: "0.0.1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: toolName, Description: "workspace isolation test tool"},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, in struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
			return &mcpsdk.CallToolResult{}, struct{}{}, nil
		})
	_ = server.Run(context.Background(), &mcpsdk.StdioTransport{})
}

// writeMCPJSON writes a workspace .mcp.json whose single stdio server is this
// test binary in server mode, exposing one tool with the given name.
func writeMCPJSON(t *testing.T, root, serverName, toolName string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	doc := map[string]any{
		"mcpServers": map[string]any{
			serverName: map[string]any{
				"command": exe,
				"env":     map[string]string{mcpServerEnv: toolName},
			},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal .mcp.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), data, 0o644); err != nil {
		t.Fatalf("write .mcp.json: %v", err)
	}
}

// newMCPTestRegistry builds a WorkspaceRegistry with MCP enabled over an empty
// base registry, hermetic against the developer's real ~/.codeagent/mcp.json
// (HOME is pointed at a temp dir).
func newMCPTestRegistry(t *testing.T, defaultRoot string) *WorkspaceRegistry {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // user-scope ~/.codeagent/mcp.json must not leak in

	wr := NewWorkspaceRegistry(defaultRoot, "")
	wr.EnableMCP(context.Background(), tools.NewRegistry(), app.Config{}, nil, false)
	t.Cleanup(func() { wr.Close() })
	return wr
}

// TestWorkspaceMCPIsolation is the core Phase-1 acceptance test: two workspaces
// with different .mcp.json files get disjoint MCP tool sets and independent
// managers, and a third workspace without any .mcp.json gets no MCP at all —
// regardless of the process launch directory.
func TestWorkspaceMCPIsolation(t *testing.T) {
	wsA, wsB, wsC := t.TempDir(), t.TempDir(), t.TempDir()
	writeMCPJSON(t, wsA, "srv_a", "alpha")
	writeMCPJSON(t, wsB, "srv_b", "beta")

	wr := newMCPTestRegistry(t, wsC)

	instA, err := wr.Get(wsA)
	if err != nil {
		t.Fatalf("Get(A): %v", err)
	}
	instB, err := wr.Get(wsB)
	if err != nil {
		t.Fatalf("Get(B): %v", err)
	}
	instC, err := wr.Get(wsC)
	if err != nil {
		t.Fatalf("Get(C): %v", err)
	}

	if instA.ToolReg == nil || instB.ToolReg == nil {
		t.Fatalf("workspaces with .mcp.json must build a ToolReg (A=%v B=%v)", instA.ToolReg, instB.ToolReg)
	}
	if _, ok := instA.ToolReg.Get("mcp__srv_a__alpha"); !ok {
		t.Errorf("A must see its own tool; registry has %v", instA.ToolReg.Names())
	}
	if _, ok := instA.ToolReg.Get("mcp__srv_b__beta"); ok {
		t.Errorf("A must NOT see B's tool (cross-workspace leak); registry has %v", instA.ToolReg.Names())
	}
	if _, ok := instB.ToolReg.Get("mcp__srv_b__beta"); !ok {
		t.Errorf("B must see its own tool; registry has %v", instB.ToolReg.Names())
	}
	if _, ok := instB.ToolReg.Get("mcp__srv_a__alpha"); ok {
		t.Errorf("B must NOT see A's tool (cross-workspace leak); registry has %v", instB.ToolReg.Names())
	}
	if instA.MCPMgr == instB.MCPMgr {
		t.Errorf("A and B must own independent MCP managers")
	}

	// No .mcp.json → no MCP manager, nil ToolReg → callers use the shared base.
	if instC.MCPMgr != nil || instC.ToolReg != nil {
		t.Errorf("workspace without .mcp.json must have no MCP (mgr=%v toolReg=%v)", instC.MCPMgr, instC.ToolReg)
	}

	// Get is a cache: the same workspace returns the same instance (and thus the
	// same MCP manager — servers are shared across conversations, not respawned).
	again, err := wr.Get(wsA)
	if err != nil {
		t.Fatalf("Get(A) again: %v", err)
	}
	if again != instA {
		t.Errorf("Get must return the cached instance for the same workspace")
	}
}

// TestWorkspaceMCPMalformedConfig: a broken .mcp.json must not make the
// workspace unusable — the instance still builds, just without MCP.
func TestWorkspaceMCPMalformedConfig(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	wr := newMCPTestRegistry(t, ws)
	inst, err := wr.Get(ws)
	if err != nil {
		t.Fatalf("Get on malformed .mcp.json must still build the workspace: %v", err)
	}
	if inst.MCPMgr != nil || inst.ToolReg != nil {
		t.Errorf("malformed config must disable MCP for the workspace, got mgr=%v", inst.MCPMgr)
	}
}

// TestWorkspaceMCPToolAllowedGate: cfg.Agent.BuiltinTools (deny-by-default
// allowlist) must gate workspace MCP tools exactly like it gates built-ins.
func TestWorkspaceMCPToolAllowedGate(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, "srv_a", "alpha")
	t.Setenv("HOME", t.TempDir())

	locked := app.Config{}
	locked.Agent.BuiltinTools = &[]string{} // nothing allowed

	wr := NewWorkspaceRegistry(ws, "")
	wr.EnableMCP(context.Background(), tools.NewRegistry(), locked, nil, false)
	t.Cleanup(func() { wr.Close() })

	inst, err := wr.Get(ws)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if inst.ToolReg == nil {
		t.Fatal("ToolReg should be built (servers configured)")
	}
	if _, ok := inst.ToolReg.Get("mcp__srv_a__alpha"); ok {
		t.Errorf("empty builtin_tools allowlist must exclude MCP tools too")
	}
}
