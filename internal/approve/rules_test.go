package approve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/settings"
)

func TestPatternFor(t *testing.T) {
	cases := map[string]string{
		"mcp__github__list_issues": "mcp__github__*", // MCP → whole server
		"mcp__db__query":           "mcp__db__*",
		"run_command":              "run_command", // built-in → exact
		"edit_file":                "edit_file",
		"mcp__weird":               "mcp__weird", // malformed (no server sep) → exact
	}
	for in, want := range cases {
		if got := patternFor(in); got != want {
			t.Errorf("patternFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// A grant persists to the project-local settings file and is reloaded by a fresh
// store — i.e. it survives "restart".
func TestGrantPersistsAndReloads(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // hermetic user scope

	s := NewRuleStore(root, nil, nil)
	rule, err := s.GrantTool("mcp__github__list_issues", ScopeProjectLocal)
	if err != nil {
		t.Fatalf("GrantTool: %v", err)
	}
	if rule != "mcp__github__*" {
		t.Fatalf("granted rule = %q, want mcp__github__*", rule)
	}

	// The file exists at the project-local path with the rule under permissions.allow.
	path := filepath.Join(root, ".codeagent", "settings.local.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings.local.json not written: %v", err)
	}
	var f settings.File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(f.Permissions.Allow) != 1 || f.Permissions.Allow[0] != "mcp__github__*" {
		t.Fatalf("persisted allow = %v, want [mcp__github__*]", f.Permissions.Allow)
	}

	// A brand-new store over the same root loads the persisted rule.
	s2 := NewRuleStore(root, nil, nil)
	if _, ok := s2.MatchAllow("mcp__github__create_pr"); !ok {
		t.Fatal("reloaded store should honor the persisted server wildcard")
	}
}

// persistAllow must not clobber unrelated keys already in the settings file.
func TestPersistPreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark","permissions":{"allow":["existing__*"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&RuleStore{}).persistAllow(path, "mcp__new__*"); err != nil {
		t.Fatalf("persistAllow: %v", err)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["theme"] != "dark" {
		t.Fatalf("unrelated key clobbered: %v", doc["theme"])
	}
	allow := toStringSlice(doc["permissions"].(map[string]any)["allow"])
	if len(allow) != 2 {
		t.Fatalf("expected existing + new rule, got %v", allow)
	}
}

// The store merges YAML config rules with both settings files (union).
func TestNewRuleStoreMergesSources(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)

	writeSettings(t, filepath.Join(home, ".codeagent", "settings.json"), "user__tool")
	writeSettings(t, filepath.Join(root, ".codeagent", "settings.json"), "shared__tool") // P11.a: project-shared layer
	writeSettings(t, filepath.Join(root, ".codeagent", "settings.local.json"), "local__tool")

	s := NewRuleStore(root, []string{"yaml__tool"}, nil)
	// yaml (layer 0) + user (1) + project-shared (2) + project-local (3) all union.
	for _, name := range []string{"yaml__tool", "user__tool", "shared__tool", "local__tool"} {
		if _, ok := s.MatchAllow(name); !ok {
			t.Errorf("store should allow %q from its source", name)
		}
	}
}

func writeSettings(t *testing.T, path, allowPattern string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"permissions":{"allow":["` + allowPattern + `"]}}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}
