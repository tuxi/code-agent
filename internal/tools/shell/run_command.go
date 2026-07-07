package shell

import (
	"bytes"
	"code-agent/internal/jobs"
	"code-agent/internal/sandbox"
	"code-agent/internal/tools"
	"code-agent/internal/truncate"
	"code-agent/internal/workspace"
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
	Policy         sandbox.CommandPolicy
	Timeout        time.Duration
	MaxOutputBytes int

	// Jobs holds background commands started with "background": true. It is
	// shared with the job_* tools so they inspect the same set. Defaulted by
	// NewRunCommandTool; buildRegistry overrides it with a shared registry.
	Jobs *jobs.Registry
}

func NewRunCommandTool() *RunCommandTool {
	return &RunCommandTool{
		Policy:         sandbox.DefaultPolicy(),
		Timeout:        120 * time.Second,
		MaxOutputBytes: 80_000,
		Jobs:           jobs.NewRegistry(),
	}
}

type runCommandInput struct {
	Command string `json:"command"`
	// Background runs the command in a job that outlives this call: Execute
	// returns a job_id immediately instead of blocking. Use it for long builds
	// and test suites; poll with job_status / job_logs, stop with job_cancel.
	Background bool `json:"background"`
}

// backgroundResult is the async shape returned when a command is launched in the
// background — there is no exit code yet, so the fields differ from a foreground
// commandResult to make the difference obvious to the model.
type backgroundResult struct {
	Command  string `json:"command"`
	JobID    string `json:"job_id"`
	Status   string `json:"status"`
	Decision string `json:"decision"`
	Note     string `json:"note"`
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
		"a few catastrophic commands are blocked. " +
		"ONE command line — NO pipes (|), redirection (>, 2>&1), or backgrounding (&). " +
		"Chain with && or ; — e.g. `go build ./... && go test ./...` or `git add .; git commit -m wip`. " +
		"Still: (1) the command runs from the workspace ROOT, so pass a path argument rather than `cd`; " +
		"(2) output is already captured and truncated for you, so DON'T pipe to head/tail/grep — just run the bare command and read the result; " +
		"(3) to filter, run the tool's own flag (e.g. `go test -run TestName`) rather than piping. " +
		`Set "background": true for a long build/test that would otherwise block — you get a job_id back immediately; then poll job_status / job_logs, or stop it with job_cancel.`
}

func (t *RunCommandTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"command": {
			Type:        "string",
			Description: `A single command to run, e.g. "go test ./..." or "git diff". No pipes/redirection/chaining.`,
		},
		"background": {
			Type:        "boolean",
			Description: `Run in the background and return a job_id immediately instead of waiting. For long builds/tests. Poll with job_status / job_logs.`,
		},
	}, "command").JSON()
}

// shellOperatorHint returns a rejection note tailored to the shell operator the
// model used, telling it the concrete single-command alternative — so it doesn't
// retry the same broken shape.
func shellOperatorHint(command string) string {
	switch {
	case strings.Contains(command, "cd "):
		return "no `cd`: commands run from the workspace root. " +
			"Pass a path instead, e.g. `go vet ./cmd/foo/` rather than `cd cmd/foo && go vet`."
	case strings.Contains(command, "|"), strings.Contains(command, "2>"), strings.Contains(command, ">"):
		return "no pipes or redirection (|, >, 2>&1): the full output is already captured and truncated for you. " +
			"Run the bare command (no `| head`/`| grep`); use the tool's own filter flag if you need less output. " +
			"Tip: you CAN chain commands with && or ; — e.g. `go build ./... && go test ./...`."
	default:
		return "pipes, redirection, and backgrounding (&) are not supported. " +
			"Tip: you CAN chain commands with && or ; — e.g. `go build ./... && go test ./...`."
	}
}

func (t *RunCommandTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in runCommandInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid run_command input: %w", err)
		}
	}
	command := strings.TrimSpace(in.Command)
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
			Decision: string(class.Decision),
			Note:     "refused by policy: " + class.Reason,
		})
	}

	// Phase A: compound commands (with && or ;) execute via sh -c with per-
	// subcommand classification already done by classifyChain(). Skip the
	// single-program guards below and delegate to the shell execution path.
	if sandbox.ContainsChainOperators(command) {
		return t.executeShell(ctx, ec, in, command, class)
	}

	// We execute a single program directly (no shell), so shell operators would
	// be passed as literal arguments and silently misbehave. Reject them clearly,
	// with a concrete fix for the operator that was used (the model retries the
	// same shape otherwise — the generic "not supported" line isn't enough).
	if sandbox.ContainsShellOperators(command) {
		return t.result(commandResult{
			Command:  command,
			ExitCode: -1,
			Decision: string(class.Decision),
			Note:     shellOperatorHint(command),
		})
	}

	args, err := sandbox.SplitArgs(command)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if len(args) == 0 {
		return tools.ToolResult{}, fmt.Errorf("empty command")
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if note := outsideWorkspaceRead(args, rootAbs); note != "" {
		return t.result(commandResult{
			Command:  command,
			ExitCode: -1,
			Decision: string(class.Decision),
			Note:     note,
		})
	}

	// Background: launch the job and return its id immediately, leaving the agent
	// free to do other work. Policy gating (block/confirm) and the operator guard
	// above still apply; only the *waiting* is removed, and the foreground timeout
	// does not bound the job.
	if in.Background {
		if t.Jobs == nil {
			t.Jobs = jobs.NewRegistry()
		}
		owner := jobs.Owner{SessionID: ec.SessionID, TurnID: ec.TurnID, CallID: ec.CallID}
		snap := t.Jobs.Start(rootAbs, command, args, owner).Snapshot()
		return t.jsonResult(backgroundResult{
			Command:  command,
			JobID:    snap.ID,
			Status:   string(snap.Status),
			Decision: string(class.Decision),
			Note:     "started in background; job_wait blocks until it finishes (preferred over polling job_status), job_logs reads output, job_cancel stops it",
		})
	}

	cmdCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	cmd.Dir = rootAbs

	var stdout, stderr bytes.Buffer
	swOut := streamWriter{buf: &stdout, stream: ec.OnStdout}
	swErr := streamWriter{buf: &stderr, stream: ec.OnStderr}
	cmd.Stdout = &swOut
	cmd.Stderr = &swErr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	res := commandResult{
		Command:    command,
		Stdout:     truncate.Head(stdout.String(), t.MaxOutputBytes),
		Stderr:     truncate.Head(stderr.String(), t.MaxOutputBytes),
		ExitCode:   0,
		DurationMS: duration.Milliseconds(),
		Decision:   string(class.Decision),
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

// executeShell runs cmd via sh -c for compound commands (Phase A). It skips the
// single-program guards (outsideWorkspaceRead, shell operator rejection) because
// safety is handled by classifyChain() during classification.
func (t *RunCommandTool) executeShell(ctx context.Context, ec tools.ExecutionContext, in runCommandInput, command string, class sandbox.Classification) (tools.ToolResult, error) {
	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	// Background: launch via sh -c in a background job.
	if in.Background {
		if t.Jobs == nil {
			t.Jobs = jobs.NewRegistry()
		}
		owner := jobs.Owner{SessionID: ec.SessionID, TurnID: ec.TurnID, CallID: ec.CallID}
		// Use sh -c to interpret the compound command.
		snap := t.Jobs.Start(rootAbs, "sh", []string{"-c", command}, owner).Snapshot()
		return t.jsonResult(backgroundResult{
			Command:  command,
			JobID:    snap.ID,
			Status:   string(snap.Status),
			Decision: string(class.Decision),
			Note:     "started in background (compound command via sh -c); job_wait blocks until it finishes, job_logs reads output, job_cancel stops it",
		})
	}

	cmdCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.Dir = rootAbs

	var stdout, stderr bytes.Buffer
	swOut := streamWriter{buf: &stdout, stream: ec.OnStdout}
	swErr := streamWriter{buf: &stderr, stream: ec.OnStderr}
	cmd.Stdout = &swOut
	cmd.Stderr = &swErr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	res := commandResult{
		Command:    command,
		Stdout:     truncate.Head(stdout.String(), t.MaxOutputBytes),
		Stderr:     truncate.Head(stderr.String(), t.MaxOutputBytes),
		ExitCode:   0,
		DurationMS: duration.Milliseconds(),
		Decision:   string(class.Decision),
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
	return t.jsonResult(res)
}

func (t *RunCommandTool) jsonResult(v any) (tools.ToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
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

// streamWriter writes to both an internal buffer (for the final result) and an
// optional streaming callback (for real-time event emission). It implements
// io.Writer so it can be assigned to cmd.Stdout / cmd.Stderr directly.
type streamWriter struct {
	buf    *bytes.Buffer
	stream func(string)
}

func (w *streamWriter) Write(p []byte) (int, error) {
	s := string(p)
	w.buf.WriteString(s)
	if w.stream != nil {
		w.stream(s)
	}
	return len(p), nil
}

func outsideWorkspaceRead(args []string, rootAbs string) string {
	if len(args) == 0 || !isReadPathCommand(args[0]) {
		return ""
	}
	for _, arg := range args[1:] {
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		target, ok := commandPathTarget(arg, rootAbs)
		if !ok {
			continue
		}
		if !workspace.IsSubPath(rootAbs, target) {
			return fmt.Sprintf("refused: %s may only read paths inside the workspace; use project tools for workspace files and respect user-scoped read limits", args[0])
		}
	}
	return ""
}

func isReadPathCommand(name string) bool {
	switch filepath.Base(name) {
	case "cat", "head", "tail", "wc", "file", "stat", "ls", "tree", "find", "grep", "rg", "sed", "awk":
		return true
	default:
		return false
	}
}

func commandPathTarget(arg, rootAbs string) (string, bool) {
	if strings.HasPrefix(arg, "~") {
		return filepath.Clean(arg), true
	}
	if filepath.IsAbs(arg) {
		return filepath.Clean(arg), true
	}
	clean := filepath.Clean(arg)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		target, err := filepath.Abs(filepath.Join(rootAbs, clean))
		if err != nil {
			return "", false
		}
		return target, true
	}
	return "", false
}

var (
	_ tools.Tool               = (*RunCommandTool)(nil)
	_ tools.SideEffecting      = (*RunCommandTool)(nil)
	_ tools.SideEffectingInput = (*RunCommandTool)(nil)
)
