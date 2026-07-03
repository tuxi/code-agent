package git

import (
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

type DiffTool struct {
	MaxBytes int
	Timeout  time.Duration
	backend  differ
}

// differ produces the raw diff text for a workspace. The exec backend shells out
// to the git binary (desktop); the go-git backend computes it in pure Go (iOS).
type differ interface {
	Diff(ctx context.Context, rootAbs string, in diffInput) (string, error)
}

type diffInput struct {
	Path   string `json:"path"`
	Staged bool   `json:"staged"`
	Stat   bool   `json:"stat"`
}

// NewDiffTool returns the desktop tool, backed by the git binary.
func NewDiffTool() *DiffTool {
	return &DiffTool{
		MaxBytes: 80_000,
		Timeout:  time.Second * 20,
		backend:  &execDiffer{timeout: time.Second * 20},
	}
}

// NewDiffToolGoGit returns the sandboxed (iOS) tool, backed by go-git so no
// subprocess is spawned. It reports the worktree's changes against the last commit
// (equivalent to `git diff HEAD`); see gogitDiffer.
func NewDiffToolGoGit() *DiffTool {
	return &DiffTool{
		MaxBytes: 80_000,
		Timeout:  time.Second * 20,
		backend:  &gogitDiffer{},
	}
}

func (t *DiffTool) Name() string {
	return "git_diff"
}

func (t *DiffTool) Description() string {
	return "Show git diff for the current workspace. This is read-only."
}

func (t *DiffTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"path": {
			Type:        "string",
			Description: `Limit the diff to this path. Empty means the whole workspace.`,
		},
		"staged": {
			Type:        "boolean",
			Description: `Show staged changes instead of working-tree changes.`,
		},
		"stat": {
			Type:        "boolean",
			Description: `Show a diffstat summary instead of the full diff.`,
		},
	}).JSON()
}

func (t *DiffTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in diffInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid git_diff input: %w", err)
		}
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	// Validate the path filter (shared across backends) before handing off.
	if strings.TrimSpace(in.Path) != "" {
		cleanPath := filepath.Clean(in.Path)
		targetAbs, err := filepath.Abs(filepath.Join(rootAbs, cleanPath))
		if err != nil {
			return tools.ToolResult{}, err
		}
		if !workspace.IsSubPath(rootAbs, targetAbs) {
			return tools.ToolResult{}, fmt.Errorf("path escapes workspace: %s", in.Path)
		}
		in.Path = filepath.ToSlash(cleanPath)
	}

	content, err := t.backend.Diff(ctx, rootAbs, in)
	if err != nil {
		return tools.ToolResult{}, err
	}

	content = strings.TrimSpace(content)
	if len(content) == 0 {
		return tools.ToolResult{Content: "No git diff."}, nil
	}
	content = truncate.Head(content, t.MaxBytes)
	return tools.ToolResult{Content: content}, nil
}

// execDiffer shells out to the git binary. It is the desktop backend.
type execDiffer struct {
	timeout time.Duration
}

func (d *execDiffer) Diff(ctx context.Context, rootAbs string, in diffInput) (string, error) {
	args := []string{"-C", rootAbs, "diff"}
	if in.Staged {
		args = append(args, "--cached")
	}
	if in.Stat {
		args = append(args, "--stat")
	}
	if in.Path != "" {
		args = append(args, "--", in.Path)
	}

	cmdCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "git", args...)
	output, err := cmd.CombinedOutput()
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("git diff timed out")
	}
	content := string(output)
	if err != nil {
		if strings.TrimSpace(content) != "" {
			return "", fmt.Errorf("git diff failed: %w\n%s", err, content)
		}
		return "", fmt.Errorf("git diff failed: %w", err)
	}
	return content, nil
}
