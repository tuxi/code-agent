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
	return projectConfigForProfile(binary, manifest, workspace, attach, reattach, baseline, "")
}

func projectConfigForProfile(binary, manifest, workspace, attach, reattach, baseline, profile string) (string, error) {
	return projectConfigWithReadWorkspaces(binary, manifest, workspace, attach, reattach, baseline, profile, nil)
}

func projectConfigWithReadWorkspaces(binary, manifest, workspace, attach, reattach, baseline, profile string, readWorkspaces []string) (string, error) {
	return projectConfigWithWorkspaceSelection(binary, manifest, workspace, attach, reattach, baseline, profile, readWorkspaces, false)
}

func projectConfigWithWorkspaceSelection(binary, manifest, workspace, attach, reattach, baseline, profile string, readWorkspaces []string, selection bool) (string, error) {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(manifest) || !filepath.IsAbs(workspace) {
		return "", errors.New("config requires absolute binary, connection-manifest and workspace paths")
	}
	if (attach != "" && reattach != "") || ((reattach == "") != (baseline == "")) {
		return "", errors.New("reattach-session requires baseline-commit and is exclusive with attach-empty-session")
	}
	if profile == "" {
		profile = "manual"
		if attach != "" || reattach != "" {
			profile = "executor"
		}
	}
	if profile != "manual" && profile != "executor" && profile != "managed" && profile != "reviewer" {
		return "", errors.New("unknown MCP profile")
	}
	if profile == "manual" && (attach != "" || reattach != "" || baseline != "") {
		return "", errors.New("manual profile uses explicit tool bindings, not fixed Session startup flags")
	}
	if len(readWorkspaces) > 16 || (len(readWorkspaces) > 0 || selection) && profile != "manual" {
		return "", errors.New("read-workspace requires manual profile and at most 16 additional roots")
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve actual workspace: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New("workspace must be an existing directory")
	}
	args := []string{"--experimental-p0a", "--profile", profile, "--connection-manifest", manifest, "--workspace", root}
	if selection {
		args = append(args, "--allow-workspace-selection")
	}
	for _, path := range readWorkspaces {
		if !filepath.IsAbs(path) {
			return "", errors.New("read-workspace must be absolute")
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", errors.New("read-workspace must be an existing directory")
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			return "", errors.New("read-workspace must be an existing directory")
		}
		args = append(args, "--read-workspace", canonical)
	}
	if attach != "" {
		args = append(args, "--attach-empty-session", attach)
	}
	if reattach != "" {
		args = append(args, "--reattach-session", reattach, "--baseline-commit", baseline)
	}
	commandJSON, _ := json.Marshal(binary)
	argsJSON, _ := json.Marshal(args)
	return fmt.Sprintf("# Save/merge at this actual worktree's .codex/config.toml, not only the original checkout.\n# Keep existing settings. Project must be trusted. No global registration.\n# Config must not violate the Bridge's clean-workspace requirement; review Git status.\n# Replace any existing required=true in this server's table: collaboration is optional.\n# Manual profile binds tasks explicitly; never replace an old task's original baseline.\n# For fixed Session profiles, disable the old task binding after delivery.\n[mcp_servers.codeagent_gui]\ncommand = %s\nargs = %s\ncwd = %s\nstartup_timeout_sec = 20\ntool_timeout_sec = 60\nenabled = true\nrequired = false\n", commandJSON, argsJSON, mustJSON(root)), nil
}

func mustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
