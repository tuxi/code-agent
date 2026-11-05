package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectConfigUsesActualWorktreeWithoutReadingGrant(t *testing.T) {
	root := t.TempDir()
	binary := "/local path/codeagent-mcp"
	// Deliberately nonexistent: config generation must not need a secret.
	manifest := "/private/not-created/connection.json"
	config, err := projectConfig(binary, manifest, root, "", "s1", strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`[mcp_servers.codeagent_gui]`, `command = "/local path/codeagent-mcp"`, `cwd = ` + mustJSON(canonical), `"--workspace","` + canonical + `"`, `"--reattach-session","s1"`, `"--experimental-p0a"`} {
		if !strings.Contains(config, part) {
			t.Fatalf("missing %s in %s", part, config)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("config generator wrote files", err)
	}
	if _, err := projectConfig(binary, manifest, "relative", "", "", ""); err == nil {
		t.Fatal("relative root accepted")
	}
	if _, err := projectConfig(binary, manifest, root, "s1", "s2", strings.Repeat("a", 40)); err == nil {
		t.Fatal("conflicting Session modes")
	}
	if _, err := projectConfig(binary, manifest, root, "", "s2", ""); err == nil {
		t.Fatal("reattach without baseline")
	}
}

func TestProjectConfigNeverRequiresCollaborationForChat(t *testing.T) {
	for _, mode := range []struct {
		name, attach, reattach, baseline string
	}{
		{name: "new"},
		{name: "empty", attach: "s1"},
		{name: "reattach", reattach: "s1", baseline: strings.Repeat("a", 40)},
	} {
		t.Run(mode.name, func(t *testing.T) {
			config, err := projectConfig("/local/codeagent-mcp", "/private/missing-grant.json", t.TempDir(), mode.attach, mode.reattach, mode.baseline)
			if err != nil {
				t.Fatal(err)
			}
			var required []string
			for _, line := range strings.Split(config, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "required") {
					required = append(required, line)
				}
			}
			if len(required) != 1 || required[0] != "required = false" {
				t.Fatalf("collaboration must explicitly remain optional: %v", required)
			}
			if !strings.Contains(config, "\nenabled = true\n") {
				t.Fatal("optional collaboration should still be enabled")
			}
		})
	}
}

func TestProjectConfigExplicitReadWorkspace(t *testing.T) {
	root, readRoot := t.TempDir(), t.TempDir()
	config, err := projectConfigWithReadWorkspaces("/local/bridge", "/private/missing-grant.json", root, "", "", "", "manual", []string{readRoot})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(readRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config, `"--read-workspace",`+mustJSON(canonical)) || !strings.Contains(config, "required = false") {
		t.Fatal(config)
	}
	for _, profile := range []string{"executor", "reviewer", "managed"} {
		if _, err := projectConfigWithReadWorkspaces("/local/bridge", "/private/missing.json", root, "", "", "", profile, []string{readRoot}); err == nil {
			t.Fatal("read scope silently accepted by", profile)
		}
	}
	if _, err := projectConfigWithReadWorkspaces("/local/bridge", "/private/missing.json", root, "", "", "", "manual", []string{"../relative"}); err == nil {
		t.Fatal("relative read workspace accepted")
	}
	for _, dir := range []string{root, readRoot} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatal("config generator changed workspace", err)
		}
	}
}

func TestProjectConfigWorkspaceSelectionIsExplicitAndManualOnly(t *testing.T) {
	root := t.TempDir()
	config, err := projectConfigWithWorkspaceSelection("/local/bridge", "/private/not-read.json", root, "", "", "", "manual", nil, true)
	if err != nil || !strings.Contains(config, `"--allow-workspace-selection"`) || !strings.Contains(config, "required = false") {
		t.Fatal(config, err)
	}
	fixed, err := projectConfigWithWorkspaceSelection("/local/bridge", "/private/not-read.json", root, "", "", "", "manual", nil, false)
	if err != nil || strings.Contains(fixed, "--allow-workspace-selection") {
		t.Fatal(fixed, err)
	}
	for _, profile := range []string{"executor", "reviewer", "managed"} {
		if _, err := projectConfigWithWorkspaceSelection("/local/bridge", "/private/not-read.json", root, "", "", "", profile, nil, true); err == nil {
			t.Fatal("selection accepted outside manual", profile)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("generator wrote workspace", err)
	}
}
