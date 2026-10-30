package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// This prints a reviewable config; it never overwrites a user's project config,
// edits Git excludes, registers globally or reads the secret-bearing manifest.
func projectConfig(binary, manifest, workspace, attach, reattach, baseline string) (string, error) {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(manifest) || !filepath.IsAbs(workspace) {
		return "", errors.New("config requires absolute binary, connection-manifest and workspace paths")
	}
	if (attach != "" && reattach != "") || ((reattach == "") != (baseline == "")) {
		return "", errors.New("reattach-session requires baseline-commit and is exclusive with attach-empty-session")
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve actual workspace: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New("workspace must be an existing directory")
	}
	args := []string{"--experimental-p0a", "--connection-manifest", manifest, "--workspace", root}
	if attach != "" {
		args = append(args, "--attach-empty-session", attach)
	}
	if reattach != "" {
		args = append(args, "--reattach-session", reattach, "--baseline-commit", baseline)
	}
	commandJSON, _ := json.Marshal(binary)
	argsJSON, _ := json.Marshal(args)
	return fmt.Sprintf("# Save/merge at this actual worktree's .codex/config.toml, not only the original checkout.\n# Keep existing settings. Project must be trusted. No global registration.\n# Config must not violate the Bridge's clean-workspace requirement; review Git status.\n[mcp_servers.codeagent_gui]\ncommand = %s\nargs = %s\ncwd = %s\nstartup_timeout_sec = 20\ntool_timeout_sec = 60\nenabled = true\n", commandJSON, argsJSON, mustJSON(root)), nil
}

func mustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
