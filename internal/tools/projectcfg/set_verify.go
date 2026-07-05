// Package projectcfg holds tools the agent uses to configure the project for
// itself — writing to the project settings layer (.codeagent/settings.json).
package projectcfg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

// SetVerifyCommandTool lets the agent configure this project's finalize-verify
// command and persist it to .codeagent/settings.local.json (P11.d) — the "agent
// configures the project for itself" capability. It writes through the settings
// package's atomic, unknown-key-preserving writer.
type SetVerifyCommandTool struct{}

func NewSetVerifyCommandTool() *SetVerifyCommandTool { return &SetVerifyCommandTool{} }

func (t *SetVerifyCommandTool) Name() string { return "set_verify_command" }

func (t *SetVerifyCommandTool) Description() string {
	return "Configure this project's verification command — the build/test run automatically at the finish line to confirm a code change before the turn ends. Persists to .codeagent/settings.local.json (machine-local, git-ignored). Omit `command` (or pass \"auto\") to detect it from the project: go.mod → `go build ./...`, Cargo.toml → `cargo build`, package.json → npm build/test, Package.swift → `swift build`. Use this when a project has no verification set up and you want future turns to catch build/test breakage automatically."
}

// SideEffects marks the tool as state-changing (it writes a settings file), so it
// is gated behind the same confirmation as other writes.
func (t *SetVerifyCommandTool) SideEffects() bool { return true }

func (t *SetVerifyCommandTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"command": {
			Type:        "string",
			Description: "The build/test command, e.g. \"go test ./...\". Omit or pass \"auto\" to detect from the project. Empty string or \"off\" disables verification.",
		},
	}).JSON()
}

type setVerifyInput struct {
	Command string `json:"command"`
}

func (t *SetVerifyCommandTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in setVerifyInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid set_verify_command input: %w", err)
		}
	}
	root := ec.WorkspaceRoot
	command := strings.TrimSpace(in.Command)

	detected := false
	if command == "" || strings.EqualFold(command, "auto") {
		command = settings.DetectVerify(root)
		detected = true
		if command == "" {
			return tools.ToolResult{Content: "No recognizable build system found (no go.mod / Cargo.toml / package.json / Package.swift). Pass an explicit `command` to configure verification."}, nil
		}
	}

	path := settings.ProjectLocalPath(root)
	if path == "" {
		return tools.ToolResult{}, fmt.Errorf("set_verify_command: no workspace root")
	}
	if err := settings.SetVerifyCommand(path, command); err != nil {
		return tools.ToolResult{}, fmt.Errorf("write settings: %w", err)
	}
	// Best-effort: keep the machine-local settings out of version control.
	_ = settings.EnsureGitignored(root, ".codeagent/settings.local.json")

	how := "set"
	if detected {
		how = "detected and set"
	}
	return tools.ToolResult{Content: fmt.Sprintf(
		"Verify command %s to %q in .codeagent/settings.local.json. Future turns that change code will run it at the finish line to confirm the change before finishing.",
		how, command)}, nil
}
