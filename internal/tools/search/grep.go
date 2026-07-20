package search

import (
	"bufio"
	"code-agent/internal/assetref"
	"code-agent/internal/tools"
	"code-agent/internal/workspace"
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
	MaxMatches   int
	MaxFileBytes int64
}

type grepInput struct {
	Query string `json:"query"`
	Path  string `json:"path"`
}

type grepMatch struct {
	Path         string `json:"path"`
	AbsolutePath string `json:"absolute_path,omitempty"`
	Line         int    `json:"line"`
	Column       int    `json:"column,omitempty"`
	Preview      string `json:"preview"`
}

type grepOutput struct {
	Kind  string           `json:"kind"`
	Query string           `json:"query"`
	Items []grepOutputItem `json:"items"`
}

type grepOutputItem struct {
	AssetID string `json:"asset_id"`
	Kind    string `json:"kind"`
	grepMatch
}

func NewGrepTool() *GrepTool {
	return &GrepTool{
		MaxMatches:   50,
		MaxFileBytes: 200_000,
	}
}

func (g *GrepTool) Name() string {
	return "grep"
}

func (g *GrepTool) Description() string {
	return "Search text in UTF-8 files under the current workspace."
}

func (g *GrepTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"query": {Type: "string", Description: "Text or pattern to search for."},
		"path":  {Type: "string", Description: `Directory to search under, relative to the workspace root. Use "." for the root.`},
	}, "query").JSON()
}

func (g *GrepTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
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

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	targetAbs := workspace.ResolveToolPath(rootAbs, in.Path)
	targetAbs, err = filepath.Abs(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}

	switch workspace.ClassifyPath(rootAbs, targetAbs) {
	case workspace.PathManagedWorktree:
		return tools.ToolResult{}, fmt.Errorf("path is not accessible: %s", in.Path)
	case workspace.PathOutsideWorkspace:
		if ec.PathAccessApprover == nil {
			return tools.ToolResult{}, fmt.Errorf("path is outside the workspace: %s", in.Path)
		}
		if !ec.PathAccessApprover.ApproveExternalPath(targetAbs, "read") {
			return tools.ToolResult{}, fmt.Errorf("access to external path was denied: %s", in.Path)
		}
		// approved — fall through to execute normally
	case workspace.PathInsideWorkspace:
		// proceed
	}

	info, err := os.Stat(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}

	var matches []grepMatch
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

			if workspace.ShouldSkipPath(rootAbs, path) || workspace.ShouldSkipName(d.Name()) {
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
		var fileMatches []grepMatch
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
	content, output, refs, err := g.result(rootAbs, ec, in.Query, matches)
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: content, Output: output, Assets: refs}, nil
}

func (g *GrepTool) searchFile(rootAbs, path, query string) ([]grepMatch, error) {

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
	var matches []grepMatch
	abs, _ := filepath.Abs(path)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	lowerQuery := strings.ToLower(query)
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		lowerLine := strings.ToLower(line)
		if idx := strings.Index(lowerLine, lowerQuery); idx >= 0 {
			matches = append(matches, grepMatch{
				Path:         rel,
				AbsolutePath: abs,
				Line:         lineNo,
				Column:       idx + 1,
				Preview:      strings.TrimSpace(line),
			})
		}
	}
	return matches, nil

}

func (g *GrepTool) result(rootAbs string, ec tools.ExecutionContext, query string, matches []grepMatch) (string, json.RawMessage, []assets.Ref, error) {
	workspaceID := assets.WorkspaceID(rootAbs)
	lines := make([]string, 0, len(matches))
	items := make([]grepOutputItem, 0, len(matches))
	refs := make([]assets.Ref, 0, len(matches))
	for i, m := range matches {
		lines = append(lines, fmt.Sprintf("%s:%d: %s", m.Path, m.Line, m.Preview))
		rng := &assets.Range{StartLine: m.Line, StartColumn: m.Column}
		id := assets.StableID(ec.TurnID, ec.CallID, i+1, "grep", m.Path, fmt.Sprint(m.Line), fmt.Sprint(m.Column), m.Preview)
		items = append(items, grepOutputItem{
			AssetID:   id,
			Kind:      "file_location",
			grepMatch: m,
		})
		refs = append(refs, assets.Ref{
			ID:                    id,
			Kind:                  "file_location",
			URI:                   assets.WorkspaceURI(workspaceID, m.Path, rng),
			DisplayName:           assets.DisplayName(m.Path, m.Line),
			WorkspaceID:           workspaceID,
			WorkspaceRelativePath: m.Path,
			AbsolutePath:          m.AbsolutePath,
			Range:                 rng,
			Preview:               m.Preview,
			MIMEType:              assets.MIMEType(m.Path),
			SourceTurnID:          ec.TurnID,
			SourceCallID:          ec.CallID,
		})
	}
	output, err := tools.JSONOutput(grepOutput{Kind: "search_results", Query: query, Items: items})
	if err != nil {
		return "", nil, nil, err
	}
	return strings.Join(lines, "\n"), output, refs, nil
}
