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
	"unicode/utf8"
)

type ReadFileTool struct {
	WorkspaceRoot string
	MaxBytes      int64
}

type readFileInput struct {
	Path string `json:"path"`
}

func NewReadFileTool(workspaceRoot string) *ReadFileTool {
	return &ReadFileTool{
		WorkspaceRoot: workspaceRoot,
		MaxBytes:      200_000,
	}
}

func (r *ReadFileTool) Name() string {
	return "read_file"
}

func (r *ReadFileTool) Description() string {
	return "Read a UTF-8 text file from the current workspace."
}

func (r *ReadFileTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"path": {
			Type:        "string",
			Description: "File path relative to the workspace root.",
		},
	}, "path").JSON()
}

func (r *ReadFileTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	var in readFileInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid read_file input: %w", err)
		}
	}

	if in.Path == "" {
		return tools.ToolResult{}, fmt.Errorf("invalid read_file input: path is required")
	}

	select {
	case <-ctx.Done():
		return tools.ToolResult{}, ctx.Err()
	default:

	}

	rootAbs, err := filepath.Abs(r.WorkspaceRoot)

	if err != nil {
		return tools.ToolResult{}, err
	}
	targetAbs := filepath.Join(rootAbs, filepath.Clean(in.Path))

	targetAbs, err = filepath.Abs(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if !workspace.IsSubPath(rootAbs, targetAbs) {
		return tools.ToolResult{}, fmt.Errorf("path escapes workspace: %s", in.Path)
	}

	info, err := os.Stat(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}

	if info.IsDir() {
		return tools.ToolResult{}, fmt.Errorf("path is a directory: %s", in.Path)
	}
	if info.Size() > r.MaxBytes {
		return tools.ToolResult{}, fmt.Errorf("file too large: %s size=%d max=%d", in.Path, info.Size(), r.MaxBytes)
	}

	data, err := os.ReadFile(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}

	if !utf8.Valid(data) {
		return tools.ToolResult{}, fmt.Errorf("file is not valid UTF-8 Text: %s", in.Path)
	}

	content := string(data)
	content = strings.Replace(content, "\r\n", "\n", -1)

	return tools.ToolResult{
		Content: content,
	}, nil
}
