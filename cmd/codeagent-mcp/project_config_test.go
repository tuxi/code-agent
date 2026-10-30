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
