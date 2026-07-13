package filesystem

import (
	"code-agent/internal/tools"
	"code-agent/internal/workspace"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type CreateFileTool struct {
	MaxBytes int64
}

type createFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func NewCreateFileTool() *CreateFileTool {
	return &CreateFileTool{
		MaxBytes: 200_000,
	}
}

func (t *CreateFileTool) Name() string {
	return "create_file"
}

func (t *CreateFileTool) Description() string {
	return "Create a new file with the given content. The file must not already exist — use edit_file to modify an existing file. Intermediate directories are created automatically."
}

func (t *CreateFileTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"path": {
			Type:        "string",
			Description: "File path relative to the workspace root.",
		},
		"content": {
			Type:        "string",
			Description: "The full content to write into the new file.",
		},
	}, "path", "content").JSON()
}

func (t *CreateFileTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in createFileInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid create_file input: %w", err)
		}
	}
	if in.Path == "" {
		return tools.ToolResult{}, fmt.Errorf("create_file input: path is required")
	}

	select {
	case <-ctx.Done():
		return tools.ToolResult{}, ctx.Err()
	default:
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}
	targetAbs := filepath.Join(rootAbs, filepath.Clean(in.Path))
	targetAbs, err = filepath.Abs(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if err := workspace.ValidatePath(rootAbs, targetAbs); err != nil {
		return tools.ToolResult{}, fmt.Errorf("path escapes workspace: %s", in.Path)
	}

	// In plan mode, only allow writes to the .codeagent/plans/ directory.
	if ec.PlanMode {
		plansDir := filepath.Join(rootAbs, ".codeagent", "plans")
		if !workspace.IsSubPath(plansDir, targetAbs) {
			return tools.ToolResult{}, fmt.Errorf(
				"plan mode: can only write to .codeagent/plans/. Use edit_file after plan approval for project files.")
		}
	}

	// File must not already exist — use edit_file for modifications.
	if info, err := os.Stat(targetAbs); err == nil {
		if info.IsDir() {
			return tools.ToolResult{Content: fmt.Sprintf("%s is a directory, not a file.", in.Path)}, nil
		}
		return tools.ToolResult{Content: fmt.Sprintf("%s already exists. Use edit_file to modify an existing file.", in.Path)}, nil
	}

	if len(in.Content) > int(t.MaxBytes) {
		return tools.ToolResult{Content: fmt.Sprintf("Content too large: %d bytes (max %d).", len(in.Content), t.MaxBytes)}, nil
	}

	// Create intermediate directories if needed.
	dir := filepath.Dir(targetAbs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tools.ToolResult{}, fmt.Errorf("create directories for %s: %w", in.Path, err)
	}

	// Normalize line endings to LF.
	content := strings.ReplaceAll(in.Content, "\r\n", "\n")

	if err := os.WriteFile(targetAbs, []byte(content), 0o644); err != nil {
		return tools.ToolResult{}, err
	}

	lineCount := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") && content != "" {
		lineCount++
	}
	return tools.ToolResult{
		Content: fmt.Sprintf("Created %s (%d lines)", in.Path, lineCount),
	}, nil
}

// SideEffects marks create_file as a mutating tool; the runtime gates it behind
// user confirmation.
func (t *CreateFileTool) SideEffects() bool { return true }

var (
	_ tools.Tool          = (*CreateFileTool)(nil)
	_ tools.SideEffecting = (*CreateFileTool)(nil)
)
