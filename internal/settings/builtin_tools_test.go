package settings

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMergeSettingsCarriesBuiltinTools is the layering regression behind the
// isolated-daemon report: the allowlist lives in a higher settings layer (the
// project file), and MergeSettings used to drop it, so the merged settings
// exposed every built-in tool.
func TestMergeSettingsCarriesBuiltinTools(t *testing.T) {
	allow := []string{"read_file"}
	base := Settings{}
	MergeSettings(&base, Settings{Agent: AgentConfig{BuiltinTools: &allow}})
	if base.Agent.BuiltinTools == nil {
		t.Fatal("overlay builtin_tools was dropped")
	}
	if !base.Agent.ToolAllowed("read_file") {
		t.Error("allowlisted read_file must be allowed")
	}
	if base.Agent.ToolAllowed("run_command") {
		t.Error("run_command must stay outside the allowlist")
	}

	// A lower layer's allowlist is inherited when the overlay does not set one.
	MergeSettings(&base, Settings{Agent: AgentConfig{MaxSteps: 3}})
	if base.Agent.BuiltinTools == nil || !base.Agent.ToolAllowed("read_file") || base.Agent.ToolAllowed("run_command") {
		t.Fatalf("inherited allowlist changed: %+v", base.Agent.BuiltinTools)
	}

	// An explicit empty allowlist in the overlay wins (deny everything).
	none := []string{}
	MergeSettings(&base, Settings{Agent: AgentConfig{BuiltinTools: &none}})
	if base.Agent.BuiltinTools == nil || len(*base.Agent.BuiltinTools) != 0 {
		t.Fatalf("empty allowlist must override: %+v", base.Agent.BuiltinTools)
	}
	if base.Agent.ToolAllowed("read_file") {
		t.Error("empty allowlist must deny every tool")
	}
}

// TestLoadKeepsProjectBuiltinTools reproduces the daemon's layered load
// (user file, then project file): an allowlist declared in the project settings
// must survive the merge.
func TestLoadKeepsProjectBuiltinTools(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codeagent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProjectSharedPath(root), []byte(`{"agent":{"builtin_tools":["web_fetch"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Load(root, home, nil)
	if cfg.Agent.BuiltinTools == nil {
		t.Fatal("project builtin_tools was dropped during layered load")
	}
	if !cfg.Agent.ToolAllowed("web_fetch") || cfg.Agent.ToolAllowed("read_file") {
		t.Fatalf("allowlist = %+v, want only web_fetch", *cfg.Agent.BuiltinTools)
	}
}
