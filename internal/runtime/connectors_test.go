package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/plugins"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

func TestConnectorSnapshotColdScopeAndPrecedence(t *testing.T) {
	wr := newMCPTestRegistry(t)
	root, other := t.TempDir(), t.TempDir()
	writeMCPJSON(t, root, "shared", "project_tool")
	writeMCPJSON(t, other, "other", "other_tool")
	body := `{"mcpServers":{"shared":{"command":"private-command-secret"}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.local.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	snap := wr.ConnectorSnapshot(root)
	if len(snap.Items) != 1 || snap.Items[0].Name != "shared" || snap.Items[0].Source != "local" || snap.Items[0].Status != "unknown" || snap.Items[0].AvailableToolCount != nil {
		t.Fatalf("cold scope/override: %+v", snap)
	}
	if len(wr.instances) != 0 {
		t.Fatal("snapshot initialized a cold workspace")
	}
	encoded, _ := json.Marshal(snap)
	if strings.Contains(string(encoded), "private-command-secret") || strings.Contains(string(encoded), "command") {
		t.Fatal("configuration secret leaked", string(encoded))
	}
}

func TestConnectorSnapshotLiveCountsFilterAndDisconnect(t *testing.T) {
	wr := newMCPTestRegistry(t)
	root := t.TempDir()
	writeMCPJSON(t, root, "srv", "alpha")
	inst, err := wr.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := wr.ConnectorSnapshot(root)
	row := snap.Items[0]
	if row.Status != "connected" || row.DiscoveredToolCount == nil || *row.DiscoveredToolCount != 1 || *row.RegisteredToolCount != 1 || *row.AvailableToolCount != 1 || row.ExposedToolCount != nil || row.ObservedAt == "" {
		t.Fatalf("live: %+v", row)
	}
	if len(row.Tools) != 1 || !row.Tools[0].Registered || row.Tools[0].CatalogVisible == nil || !*row.Tools[0].CatalogVisible {
		t.Fatalf("tool origin: %+v", row.Tools)
	}
	if err := inst.MCPMgr.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		row = wr.ConnectorSnapshot(root).Items[0]
		if row.Status == "disconnected" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if row.Status != "disconnected" || *row.AvailableToolCount != 0 || *row.RegisteredToolCount != 1 {
		t.Fatalf("stale startup report: %+v", row)
	}
}

func TestConnectorSnapshotPolicyDiagnosticAndReload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	writeMCPJSON(t, root, "srv", "alpha")
	cfg := settings.Settings{Profile: settings.ProfileSandboxed}
	wr := NewWorkspaceRegistry("")
	wr.EnableMCP(context.Background(), tools.NewRegistry(), cfg, nil, false)
	t.Cleanup(func() { _ = wr.Close() })
	inst, err := wr.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := wr.ConnectorSnapshot(root)
	if len(snap.Items) != 1 || snap.Items[0].Status != "disabled" || snap.Items[0].Reason != "transport_not_supported_by_profile" {
		t.Fatalf("silent filtering: %+v", snap)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.local.json"), []byte(`{broken-config-secret`), 0600); err != nil {
		t.Fatal(err)
	}
	if inst.ReloadMCP() {
		t.Fatal("invalid reload succeeded")
	}
	snap = wr.ConnectorSnapshot(root)
	encoded, _ := json.Marshal(snap)
	if snap.State != "stale" || len(snap.Items) != 1 || len(snap.Diagnostics) == 0 || strings.Contains(string(encoded), "broken-config-secret") {
		t.Fatalf("bad configuration dropped prior snapshot or leaked details: %s", encoded)
	}
}

func TestConnectorSnapshotAllowlistCountsAndBusyReload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	writeMCPJSON(t, root, "srv", "alpha")
	cfg := settings.Settings{}
	cfg.Agent.BuiltinTools = &[]string{}
	wr := NewWorkspaceRegistry("")
	wr.EnableMCP(context.Background(), tools.NewRegistry(), cfg, nil, false)
	t.Cleanup(func() { _ = wr.Close() })
	inst, err := wr.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	row := wr.ConnectorSnapshot(root).Items[0]
	if *row.DiscoveredToolCount != 1 || *row.RegisteredToolCount != 0 || *row.AvailableToolCount != 0 || row.Tools[0].Registered {
		t.Fatalf("allowlist ignored: %+v", row)
	}
	inst.mu.Lock()
	snap := wr.ConnectorSnapshot(root)
	inst.mu.Unlock()
	if snap.State != "loading" {
		t.Fatal("read waited on reload")
	}
}

func TestConnectorSnapshotReloadKeepsPluginServers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	skillsDir := filepath.Join(t.TempDir(), "skills")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source := writeManagementPlugin(t, "docs", "1.0.0", "Docs", map[string]any{"lookup": map[string]any{"command": exe, "env": map[string]string{mcpServerEnv: "lookup"}}})
	if _, err := plugins.InstallLocal(skillsDir, source, plugins.InstallOptions{Confirmed: true, Source: source}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeMCPJSON(t, root, "project", "alpha")
	wr := NewWorkspaceRegistry(skillsDir)
	wr.EnableMCP(context.Background(), tools.NewRegistry(), settings.Settings{GlobalSkillsDir: skillsDir}, nil, false)
	t.Cleanup(func() { _ = wr.Close() })
	inst, err := wr.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	if snap := wr.ConnectorSnapshot(root); len(snap.Items) != 2 {
		t.Fatalf("initial: %+v", snap)
	}
	writeMCPJSON(t, root, "new_project", "beta")
	changed := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, ".mcp.json"), changed, changed); err != nil {
		t.Fatal(err)
	}
	if !inst.ReloadMCP() {
		t.Fatal("reload did not run")
	}
	snap := wr.ConnectorSnapshot(root)
	if len(snap.Items) != 2 {
		t.Fatalf("reload dropped plugin: %+v", snap)
	}
	found := false
	for _, row := range snap.Items {
		if row.Name == "docs__lookup" {
			found = true
			if row.Source != "plugin" || row.Status != "connected" || *row.RegisteredToolCount != 1 {
				t.Fatalf("plugin source/tool binding lost: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("plugin MCP disappeared on reload")
	}
}

func TestConnectorSnapshotFailedConnectionRetainsUnknownDiscovery(t *testing.T) {
	wr := newMCPTestRegistry(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"broken":{"command":"/missing/executable-secret"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := wr.Get(root); err != nil {
		t.Fatal(err)
	}
	snap := wr.ConnectorSnapshot(root)
	if len(snap.Items) != 1 || snap.Items[0].Status != "failed" || snap.Items[0].DiscoveredToolCount != nil || snap.Items[0].AvailableToolCount == nil || *snap.Items[0].AvailableToolCount != 0 {
		t.Fatalf("failure vanished or discovery fabricated: %+v", snap)
	}
	encoded, _ := json.Marshal(snap)
	if strings.Contains(string(encoded), "executable-secret") {
		t.Fatal("raw connection error leaked")
	}
}
