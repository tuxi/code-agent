package filesystem

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ListFilesTool struct {
	WorkspaceRoot string
	MaxDepth      int
}

type listFilesInput struct {
	Path string `json:"path"`
}

func NewListFilesTool(workspaceRoot string) *ListFilesTool {
	return &ListFilesTool{
		WorkspaceRoot: workspaceRoot,
		MaxDepth:      3,
	}
}

func (l ListFilesTool) Name() string {
	return "list_files"
}

func (l ListFilesTool) Description() string {
	return "List files and directories under a path in the current workspace."
}

func (l ListFilesTool) InputSchema() string {
	return `
{"path":"relative path under workspace, default is ."}`
}

func (t *ListFilesTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {

	var in listFilesInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid list_files input: %w", err)
		}
	}
	if in.Path == "" {
		in.Path = "."
	}
	rootAbs, err := filepath.Abs(t.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}
	targetAbs := filepath.Join(rootAbs, filepath.Clean(in.Path))
	targetAbs, err = filepath.Abs(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if !isSubPath(rootAbs, targetAbs) {
		return tools.ToolResult{}, fmt.Errorf("path escapes workspace: %s", in.Path)
	}
	info, err := os.Stat(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if !info.IsDir() {
		return tools.ToolResult{}, fmt.Errorf("path is not a directory: %s", in.Path)
	}
	var entries []string
	err = filepath.WalkDir(targetAbs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		name := d.Name()
		if shouldSkip(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relFromTarget, err := filepath.Rel(targetAbs, path)
		if err != nil {
			return nil
		}
		if relFromTarget == "." {
			return nil
		}
		depth := pathDepth(relFromTarget)
		if depth > t.MaxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relFromRoot, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return nil
		}
		item := filepath.ToSlash(relFromRoot)
		if d.IsDir() {
			item += "/"
		}
		entries = append(entries, item)
		return nil
	})
	if err != nil {
		return tools.ToolResult{}, err
	}
	sort.Strings(entries)
	if len(entries) == 0 {
		return tools.ToolResult{Content: "(empty)"}, nil
	}
	return tools.ToolResult{
		Content: strings.Join(entries, "\n"),
	}, nil

}

func shouldSkip(name string) bool {

	switch name {
	case ".git", "node_modules", "vendor", ".idea", ".vscode", ".DS_Store", "dist", "build", ".next":
		return true
	default:
		return false
	}

}

func pathDepth(rel string) int {

	if rel == "." || rel == "" {
		return 0
	}
	return strings.Count(filepath.ToSlash(rel), "/") + 1

}

func isSubPath(rootAbs, targetAbs string) bool {

	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")

}
