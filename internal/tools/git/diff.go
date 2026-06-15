package git

import (
	"code-agent/internal/tools"
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

type DiffTool struct {
	WorkspaceRoot string
	MaxBytes      int
	Timeout       time.Duration
}

type diffInput struct {
	Path   string `json:"path"`
	Staged bool   `json:"staged"`
	Stat   bool   `json:"stat"`
}

func NewDiffTool(workspaceRoot string) *DiffTool {
	return &DiffTool{
		WorkspaceRoot: workspaceRoot,
		MaxBytes:      80_000,
		Timeout:       time.Second * 20,
	}
}

func (t *DiffTool) Name() string {
	return "git_diff"
}

func (t *DiffTool) Description() string {
	return "Show git diff for the current workspace. This is read-only."
}

func (t *DiffTool) InputSchema() string {
	return `{"path":"optional relative path under workspace","staged":false,"stat":false}`
}

func (t *DiffTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	var in diffInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid git_diff input: %w", err)
		}
	}

	rootAbs, err := filepath.Abs(t.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	args := []string{"-C", rootAbs, "diff"}
	if in.Staged {
		args = append(args, "--cached")
	}
	if in.Stat {
		args = append(args, "--stat")
	}

	if strings.TrimSpace(in.Path) != "" {
		cleanPath := filepath.Clean(in.Path)
		targetAbs := filepath.Join(rootAbs, cleanPath)
		targetAbs, err = filepath.Abs(targetAbs)
		if err != nil {
			return tools.ToolResult{}, err
		}
		if !workspace.IsSubPath(rootAbs, targetAbs) {
			return tools.ToolResult{}, fmt.Errorf("path escapes workspace: %s", in.Path)
		}
		args = append(args, "--", filepath.ToSlash(cleanPath))
	}

	cmdCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "git", args...)

	output, err := cmd.CombinedOutput()

	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return tools.ToolResult{}, fmt.Errorf("git diff timed out")
	}

	content := string(output)
	if err != nil {
		if strings.TrimSpace(content) != "" {
			return tools.ToolResult{}, fmt.Errorf("git diff failed: %w\n%s", err, content)
		}
		return tools.ToolResult{}, fmt.Errorf("git diff failed: %w", err)
	}

	content = strings.TrimSpace(content)
	if len(content) == 0 {
		return tools.ToolResult{
			Content: "No git diff.",
		}, nil
	}

	if len(content) > t.MaxBytes {
		content = content[:t.MaxBytes] + "\n...<truncated>"
	}

	return tools.ToolResult{
		Content: content,
	}, nil
}
