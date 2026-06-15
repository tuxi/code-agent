package search

import (
	"bufio"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type GrepTool struct {
	WorkspaceRoot string
	MaxMatches    int
	MaxFileBytes  int64
}

type grepInput struct {
	Query string `json:"query"`
	Path  string `json:"path"`
}

func NewGrepTool(workspaceRoot string) *GrepTool {
	return &GrepTool{
		WorkspaceRoot: workspaceRoot,
		MaxMatches:    50,
		MaxFileBytes:  200_000,
	}
}

func (g *GrepTool) Name() string {
	return "grep"
}

func (g *GrepTool) Description() string {
	return "Search text in UTF-8 files under the current workspace."
}

func (g *GrepTool) InputSchema() string {
	return `{"query":"text to search","path":"relative directory or file path, default is ."}`
}

func (g *GrepTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	var in grepInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid grep input: %w", err)
		}
	}

	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return tools.ToolResult{}, fmt.Errorf("missing query")
	}

	if in.Path == "" {
		in.Path = "."
	}

	rootAbs, err := filepath.Abs(g.WorkspaceRoot)
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

	var matches []string
	if info.IsDir() {
		err = filepath.WalkDir(targetAbs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			if len(matches) > g.MaxMatches {
				return filepath.SkipDir
			}

			if shouldSkip(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if d.IsDir() {
				return nil
			}

			fileMatches, err := g.searchFile(rootAbs, path, in.Query)
			if err == nil {
				matches = append(matches, fileMatches...)
			}
			return nil
		})
	} else {
		var fileMatches []string
		fileMatches, err = g.searchFile(rootAbs, targetAbs, in.Query)
		if err == nil {
			matches = append(matches, fileMatches...)
		}
	}

	if err != nil {
		return tools.ToolResult{}, err
	}

	if len(matches) > g.MaxMatches {
		matches = matches[:g.MaxMatches]
	}
	if len(matches) == 0 {
		return tools.ToolResult{
			Content: "No matches.",
		}, nil
	}
	return tools.ToolResult{
		Content: strings.Join(matches, "\n"),
	}, nil
}

func (g *GrepTool) searchFile(rootAbs, path, query string) ([]string, error) {

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > g.MaxFileBytes {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, nil
	}
	rel, err := filepath.Rel(rootAbs, path)
	if err != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)
	var matches []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.Contains(strings.ToLower(line), strings.ToLower(query)) {
			matches = append(matches, fmt.Sprintf("%s:%d: %s", rel, lineNo, strings.TrimSpace(line)))
		}
	}
	return matches, nil

}

func shouldSkip(name string) bool {

	switch name {
	case ".git", "node_modules", "vendor", ".idea", ".vscode", ".DS_Store", "dist", "build", ".next":
		return true
	default:
		return false
	}

}

func isSubPath(rootAbs, targetAbs string) bool {

	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")

}
