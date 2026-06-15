package shell

import (
	"bytes"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type RunCommandTool struct {
	WorkspaceRoot   string
	AllowedCommands []string
	Timeout         time.Duration
	MaxOutputBytes  int
}

func NewRunCommandTool(workspaceRoot string) *RunCommandTool {
	return &RunCommandTool{
		WorkspaceRoot: workspaceRoot,
		AllowedCommands: []string{
			"go test ./...",
			"go test ./internal/...",
			"go test ./cmd/...",
			"go vet ./...",
			"git status",
		},
		Timeout:        60 * time.Second,
		MaxOutputBytes: 80_000,
	}
}

type runCommandInput struct {
	Command string `json:"command"`
}

func (t *RunCommandTool) Name() string {
	return "run_command"
}

func (t *RunCommandTool) Description() string {
	return "Run an allowlisted command in the workspace, such as go test or git status."
}

func (t *RunCommandTool) InputSchema() string {
	return `{"command":"allowlisted command to run, such as go test ./..."}`
}

func (t *RunCommandTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	var in runCommandInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid run_command input: %w", err)
		}
	}

	command := strings.TrimSpace(in.Command)
	if len(command) == 0 {
		return tools.ToolResult{}, fmt.Errorf("command is required")
	}

	if !t.isAllowed(command) {
		return tools.ToolResult{}, fmt.Errorf("command is not allowed: %s", command)
	}

	rootAbs, err := filepath.Abs(t.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	args := splitCommand(in.Command)
	if len(args) == 0 {
		return tools.ToolResult{}, fmt.Errorf("empty command")
	}

	cmdCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	cmd.Dir = rootAbs

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return tools.ToolResult{}, fmt.Errorf("command timed out after %s: %s", t.Timeout, command)
	}

	output := formatCommandOutput(command, stdout.String(), stderr.String(), err)
	output = truncateOutput(output, t.MaxOutputBytes)
	return tools.ToolResult{
		Content: output,
	}, nil
}

func formatCommandOutput(command, stdout, stderr string, err error) string {
	var b strings.Builder
	b.WriteString("Command: ")
	b.WriteString(command)
	b.WriteString("\n")
	if err != nil {
		b.WriteString("Status: failed\n")
		b.WriteString("Error: ")
		b.WriteString(err.Error())
		b.WriteString("\n")
	} else {
		b.WriteString("Status: success\n")
	}
	if strings.TrimSpace(stdout) != "" {
		b.WriteString("\nSTDOUT:\n")
		b.WriteString(stdout)
	}
	if strings.TrimSpace(stderr) != "" {
		b.WriteString("\nSTDERR:\n")
		b.WriteString(stderr)
	}
	return b.String()

}

func (t *RunCommandTool) isAllowed(command string) bool {
	command = strings.TrimSpace(command)
	for _, item := range t.AllowedCommands {
		if command == item {
			return true
		}
	}
	return false
}

func splitCommand(cmd string) []string {
	// P2 第一版只支持简单命令，不支持引号、管道、重定向、&&、||
	// 因为 allowlist 是精确匹配，所以这里用 Fields 足够安全。
	return strings.Fields(cmd)
}

func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...<truncated>"
}

var _ tools.Tool = (*RunCommandTool)(nil)
