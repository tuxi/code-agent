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

	cmdCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	// --recount的作用是：当 patch 是手写或模型生成的，hunk 行数可能不准时，让 git 重新计算 hunk 行数。
	args := []string{"-C", rootAbs, "apply", "--recount"}
	if !in.Apply {
		args = append(args, "--check")
	}
	cmd := exec.CommandContext(cmdCtx, "git", args...)
	cmd.Stdin = strings.NewReader(patch)

	output, err := cmd.CombinedOutput()

	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return tools.ToolResult{}, fmt.Errorf("git apply --check timed out")
	}

	content := strings.TrimSpace(string(output))
	if err != nil {
		if content != "" {
			return tools.ToolResult{
				Content: "Patch validation failed:\n" + content,
			}, nil
		}
		return tools.ToolResult{}, fmt.Errorf("git apply --check failed: %w", err)
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

var _ tools.Tool = (*ApplyPatchTool)(nil)
