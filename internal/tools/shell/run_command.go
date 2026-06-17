package shell

import (
	"bytes"
	"code-agent/internal/sandbox"
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

// RunCommandTool runs a shell command in the workspace. It is a controlled
// system-shell layer rather than an open one: every command is classified by a
// sandbox.CommandPolicy into allow / confirm / block. Allowed (read-only and
// build) commands run directly; mutating commands are gated behind the agent's
// confirmation prompt (via SideEffectsFor); catastrophic commands are refused
// outright. The result is structured (stdout, stderr, exit code, duration) so
// the model can act on it.
type RunCommandTool struct {
	WorkspaceRoot  string
	Policy         sandbox.CommandPolicy
	Timeout        time.Duration
	MaxOutputBytes int
}

func NewRunCommandTool(workspaceRoot string) *RunCommandTool {
	return &RunCommandTool{
		WorkspaceRoot:  workspaceRoot,
		Policy:         sandbox.DefaultPolicy(),
		Timeout:        120 * time.Second,
		MaxOutputBytes: 80_000,
	}
}

type runCommandInput struct {
	Command string `json:"command"`
}

// commandResult is the structured output of a run_command call. It is marshaled
// to JSON so the model receives machine-actionable fields rather than a prose
// blob: exit_code drives success/failure, duration_ms is observability, and
// decision/note explain any policy action.
type commandResult struct {
	Command    string `json:"command"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Decision   string `json:"decision"`       // allow | confirm | block
	Note       string `json:"note,omitempty"` // policy reason, timeout, or exec error
}

func (t *RunCommandTool) Name() string {
	return "run_command"
}

func (t *RunCommandTool) Description() string {
	return "Run a command in the workspace and get structured output (stdout, stderr, exit_code, duration_ms). " +
		"Read-only and build commands (ls, cat, grep, git status/diff/log, go build/test/vet, cargo check) run directly; " +
		"commands that mutate the tree or reach the network (rm, mv, curl, git checkout/commit/push) require user confirmation; " +
		"a few catastrophic commands are blocked. One command per call — pipes, redirection, and chaining (|, >, &&) are not supported."
}

func (t *RunCommandTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"command": {
			Type:        "string",
			Description: `A single command to run, e.g. "go test ./..." or "git diff". No pipes/redirection/chaining.`,
		},
	}, "command").JSON()
}

func (t *RunCommandTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	command, err := parseCommand(input)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if command == "" {
		return tools.ToolResult{}, fmt.Errorf("command is required")
	}

	class := t.Policy.Classify(command)

	// Blocked commands never run. Surface a structured refusal rather than a hard
	// error so the model can read the reason and choose a different approach.
	if class.Decision == sandbox.Block {
		return t.result(commandResult{
			Command:  command,
			ExitCode: -1,
			Decision: class.Decision.String(),
			Note:     "refused by policy: " + class.Reason,
		})
	}

	// We execute a single program directly (no shell), so shell operators would
	// be passed as literal arguments and silently misbehave. Reject them clearly.
	if sandbox.ContainsShellOperators(command) {
		return t.result(commandResult{
			Command:  command,
			ExitCode: -1,
			Decision: class.Decision.String(),
			Note:     "pipes, redirection, and chaining are not supported; run one command at a time",
		})
	}

	args, err := sandbox.SplitArgs(command)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if len(args) == 0 {
		return tools.ToolResult{}, fmt.Errorf("empty command")
	}

	rootAbs, err := filepath.Abs(t.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	cmdCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	cmd.Dir = rootAbs

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	res := commandResult{
		Command:    command,
		Stdout:     truncate(stdout.String(), t.MaxOutputBytes),
		Stderr:     truncate(stderr.String(), t.MaxOutputBytes),
		ExitCode:   0,
		DurationMS: duration.Milliseconds(),
		Decision:   class.Decision.String(),
	}

	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		res.ExitCode = -1
		res.Note = fmt.Sprintf("timed out after %s", t.Timeout)
		return t.result(res)
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			// Command could not start (not found, not executable, ...).
			res.ExitCode = -1
			res.Note = runErr.Error()
		}
	}

	return t.result(res)
}

// SideEffectsFor makes run_command's confirmation gate command-aware: allowed
// (read-only/build) commands and blocked commands do not prompt — the former
// because they are safe, the latter because Execute refuses them anyway — while
// mutating and unrecognized commands require confirmation.
func (t *RunCommandTool) SideEffectsFor(input json.RawMessage) bool {
	command, err := parseCommand(input)
	if err != nil || command == "" {
		return false // Execute will return the error; no need to prompt.
	}
	return t.Policy.Classify(command).Decision == sandbox.Confirm
}

// SideEffects keeps the static marker (always true) as a conservative fallback
// for any caller that gates without the input. The loop uses SideEffectsFor.
func (t *RunCommandTool) SideEffects() bool { return true }

func (t *RunCommandTool) result(res commandResult) (tools.ToolResult, error) {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: string(data)}, nil
}

func parseCommand(input json.RawMessage) (string, error) {
	var in runCommandInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid run_command input: %w", err)
		}
	}
	return strings.TrimSpace(in.Command), nil
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "\n...<truncated>"
}

var (
	_ tools.Tool               = (*RunCommandTool)(nil)
	_ tools.SideEffecting      = (*RunCommandTool)(nil)
	_ tools.SideEffectingInput = (*RunCommandTool)(nil)
)
