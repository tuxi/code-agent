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
	WorkspaceRoot string
	MaxBytes      int
	Timeout       time.Duration
}

type applyPatchInput struct {
	Patch string `json:"patch"`
	// Apply 是否应用，逻辑是：
	// Apply=false：git apply --check --recount
	// Apply=true：git apply --recount
	// 注意：即使工具支持 apply=true，也不要在 system prompt 里暴露给模型。应用 patch 必须由 Runtime 控制。
	Apply bool `json:"apply"`
}

func NewApplyPatchTool(workspaceRoot string) *ApplyPatchTool {
	return &ApplyPatchTool{
		WorkspaceRoot: workspaceRoot,
		MaxBytes:      200_000,
		Timeout:       30 * time.Second,
	}
}

func (t *ApplyPatchTool) Name() string {
	return "apply_patch"
}

func (t *ApplyPatchTool) Description() string {
	return "Validate or apply a unified diff patch using git apply. Applying modifies files and must be confirmed by the runtime."
}

func (t *ApplyPatchTool) InputSchema() string {
	return `{"patch":"unified diff patch content","apply":false}`
}

func (t *ApplyPatchTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
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

	rootAbs, err := filepath.Abs(t.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	// First try git apply default behavior.
	// Git apply defaults to stripping one leading path component, which works for a/... and b/... patches.
	result := t.runGitApply(ctx, rootAbs, patch, in.Apply, "")

	// If that fails, retry with -p0.
	// This supports patches whose headers are like:
	// --- internal/ui/confirm.go
	// +++ internal/ui/confirm.go
	if result.Err != nil {
		fallback := t.runGitApply(ctx, rootAbs, patch, in.Apply, "-p0")
		if fallback.Err == nil {
			if in.Apply {
				return tools.ToolResult{
					Content: "Patch applied successfully. Applied with -p0 path mode.",
				}, nil
			}
			return tools.ToolResult{
				Content: "Patch applies cleanly. Validated with -p0 path mode.",
			}, nil
		}

		// Both attempts failed. Return both errors to help the model fix the patch.
		defaultMsg := strings.TrimSpace(result.Content)
		fallbackMsg := strings.TrimSpace(fallback.Content)

		var b strings.Builder
		b.WriteString("Patch validation failed.\n")
		if in.Apply {
			b.Reset()
			b.WriteString("Patch apply failed.\n")
		}

		b.WriteString("\nDefault path mode failed")
		if defaultMsg != "" {
			b.WriteString(":\n")
			b.WriteString(defaultMsg)
		} else {
			b.WriteString(": ")
			b.WriteString(result.Err.Error())
		}

		b.WriteString("\n\n-p0 path mode failed")
		if fallbackMsg != "" {
			b.WriteString(":\n")
			b.WriteString(fallbackMsg)
		} else {
			b.WriteString(": ")
			b.WriteString(fallback.Err.Error())
		}

		return tools.ToolResult{
			Content: b.String(),
		}, nil
	}

	if in.Apply {
		return tools.ToolResult{
			Content: "Patch applied successfully.",
		}, nil
	}

	return tools.ToolResult{
		Content: "Patch applies cleanly.",
	}, nil
}

type applyPatchExecResult struct {
	Content string
	Err     error
}

func (t *ApplyPatchTool) runGitApply(ctx context.Context, rootAbs string, patch string, apply bool, stripOption string) applyPatchExecResult {
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
		return applyPatchExecResult{
			Err: fmt.Errorf("git apply timed out"),
		}
	}

	content := strings.TrimSpace(string(output))

	if err != nil {
		if content != "" {
			return applyPatchExecResult{
				Content: content,
				Err:     err,
			}
		}
		return applyPatchExecResult{
			Err: err,
		}
	}

	return applyPatchExecResult{
		Content: content,
		Err:     nil,
	}
}

var _ tools.Tool = (*ApplyPatchTool)(nil)
