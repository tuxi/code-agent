package git

import (
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

type ApplyPatchTool struct {
	MaxBytes int
	Timeout  time.Duration
}

type applyPatchInput struct {
	Patch string `json:"patch"`
}

func NewApplyPatchTool() *ApplyPatchTool {
	return &ApplyPatchTool{
		MaxBytes: 200_000,
		Timeout:  30 * time.Second,
	}
}

func (t *ApplyPatchTool) Name() string { return "apply_patch" }

func (t *ApplyPatchTool) Description() string {
	return "Apply a unified diff patch to the workspace. The patch is validated with a dry run first; if it does not apply cleanly, nothing is changed and the error is returned so you can fix the patch and try again."
}

func (t *ApplyPatchTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"patch": {
			Type:        "string",
			Description: "A unified diff (git-style) describing the change to make.",
		},
	}, "patch").JSON()
}

// SideEffects marks apply_patch as a mutating tool, so the runtime gates it
// behind user confirmation before it runs.
func (t *ApplyPatchTool) SideEffects() bool { return true }

func (t *ApplyPatchTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in applyPatchInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid apply_patch input: %w", err)
		}
	}

	patch := strings.TrimSpace(in.Patch)
	if patch == "" {
		return tools.ToolResult{}, fmt.Errorf("patch input is required")
	}
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	if len(patch) > t.MaxBytes {
		return tools.ToolResult{}, fmt.Errorf("patch too large: size=%d max=%d", len(patch), t.MaxBytes)
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	// 1. Validate with a dry run (git apply --check). If it fails, change nothing
	//    and hand the error back so the model can fix the patch and retry.
	if ok, msg := t.runWithFallback(ctx, rootAbs, patch, false); !ok {
		return tools.ToolResult{
			Content: "Patch did not validate; no files were changed.\n\n" + msg,
		}, nil
	}

	// 2. Apply for real.
	if ok, msg := t.runWithFallback(ctx, rootAbs, patch, true); !ok {
		return tools.ToolResult{
			Content: "Patch validated but failed to apply; no files were changed.\n\n" + msg,
		}, nil
	}

	return tools.ToolResult{Content: "Patch applied successfully."}, nil
}

// runWithFallback runs git apply (check or apply) trying the default path mode
// first and then -p0, to support both a/.. b/.. headers and bare paths. It
// returns whether the operation succeeded and a human-readable message.
func (t *ApplyPatchTool) runWithFallback(ctx context.Context, rootAbs, patch string, apply bool) (bool, string) {
	primary := t.runGitApply(ctx, rootAbs, patch, apply, "")
	if primary.Err == nil {
		return true, primary.Content
	}

	fallback := t.runGitApply(ctx, rootAbs, patch, apply, "-p0")
	if fallback.Err == nil {
		return true, fallback.Content
	}

	var b strings.Builder
	b.WriteString("Default path mode failed")
	if msg := strings.TrimSpace(primary.Content); msg != "" {
		b.WriteString(":\n" + msg)
	} else {
		b.WriteString(": " + primary.Err.Error())
	}
	b.WriteString("\n\n-p0 path mode failed")
	if msg := strings.TrimSpace(fallback.Content); msg != "" {
		b.WriteString(":\n" + msg)
	} else {
		b.WriteString(": " + fallback.Err.Error())
	}
	return false, b.String()
}

type applyPatchExecResult struct {
	Content string
	Err     error
}

func (t *ApplyPatchTool) runGitApply(ctx context.Context, rootAbs, patch string, apply bool, stripOption string) applyPatchExecResult {
	args := []string{"-C", rootAbs, "apply", "--recount"}
	if !apply {
		args = append(args, "--check")
	}
	if stripOption != "" {
		args = append(args, stripOption)
	}

	cmdCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "git", args...)
	cmd.Stdin = strings.NewReader(patch)

	output, err := cmd.CombinedOutput()
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return applyPatchExecResult{Err: fmt.Errorf("git apply timed out")}
	}

	content := strings.TrimSpace(string(output))
	if err != nil {
		if content != "" {
			return applyPatchExecResult{Content: content, Err: err}
		}
		return applyPatchExecResult{Err: err}
	}
	return applyPatchExecResult{Content: content, Err: nil}
}

var (
	_ tools.Tool          = (*ApplyPatchTool)(nil)
	_ tools.SideEffecting = (*ApplyPatchTool)(nil)
)
