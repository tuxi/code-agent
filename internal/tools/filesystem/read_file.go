package filesystem

import (
	"code-agent/internal/tools"
	"code-agent/internal/workspace"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

type ReadFileTool struct {
	WorkspaceRoot string
	MaxBytes      int64
}

// lineNumber accepts both a JSON number (42) and a JSON string containing a
// number ("42"). Providers sometimes pass numeric fields as quoted strings,
// which would cause json.Unmarshal into int to fail.
type lineNumber int

func (l *lineNumber) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*l = lineNumber(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("line number must be a number or string number, got %s", string(data))
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid line number %q: %w", s, err)
	}
	*l = lineNumber(n)
	return nil
}

type readFileInput struct {
	Path   string     `json:"path"`
	Offset lineNumber `json:"offset,omitempty"`
	Limit  lineNumber `json:"limit,omitempty"`
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
		"offset": {
			Type:        "integer",
			Description: "Optional. Line number (1-based) to start reading from. Default reads from line 1. Also accepts string numbers.",
		},
		"limit": {
			Type:        "integer",
			Description: "Optional. Maximum number of lines to read. Default reads to end of file. Also accepts string numbers.",
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

	// Apply offset (line-based, 1-indexed).
	lines := strings.Split(content, "\n")
	startLine := 0
	if in.Offset > 0 {
		startLine = int(in.Offset) - 1 // 1-based → 0-indexed
	}
	if startLine >= len(lines) {
		return tools.ToolResult{Content: ""}, nil
	}
	if startLine < 0 {
		startLine = 0
	}
	lines = lines[startLine:]

	// Apply limit (line count).
	if in.Limit > 0 {
		n := int(in.Limit)
		if n < len(lines) {
			lines = lines[:n]
		}
	}

	content = strings.Join(lines, "\n")

	return tools.ToolResult{
		Content: content,
	}, nil
}
