package filesystem

import (
	"code-agent/internal/assetref"
	"code-agent/internal/tools"
	"code-agent/internal/workspace"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ListFilesTool struct {
	MaxDepth int
}

type listFilesInput struct {
	Path string `json:"path"`
}

type listFilesOutput struct {
	Kind         string                `json:"kind"`
	Path         string                `json:"path"`
	AbsolutePath string                `json:"absolute_path,omitempty"`
	Items        []listFilesOutputItem `json:"items"`
}

type listFilesOutputItem struct {
	AssetID      string `json:"asset_id"`
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	AbsolutePath string `json:"absolute_path,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	MIMEType     string `json:"mime_type,omitempty"`
}

type listFilesEntry struct {
	rel     string
	abs     string
	kind    string
	display string
	mime    string
}

func NewListFilesTool() *ListFilesTool {
	return &ListFilesTool{
		MaxDepth: 3,
	}
}

func (l *ListFilesTool) Name() string {
	return "list_files"
}

func (l *ListFilesTool) Description() string {
	return "List files and directories under a path in the current workspace."
}

func (l *ListFilesTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"path": {
			Type:        "string",
			Description: `Directory path relative to the workspace root. Use "." for the root.`,
		},
	}).JSON()
}

func (l *ListFilesTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {

	var in listFilesInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid list_files input: %w", err)
		}
	}
	if in.Path == "" {
		in.Path = "."
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
	info, err := os.Stat(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if !info.IsDir() {
		return tools.ToolResult{}, fmt.Errorf("path is not a directory: %s", in.Path)
	}
	targetRel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		targetRel = filepath.Clean(in.Path)
	}
	targetRel = filepath.ToSlash(targetRel)
	if targetRel == "." {
		targetRel = "."
	}

	var entries []listFilesEntry
	err = filepath.WalkDir(targetAbs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		name := d.Name()
		if workspace.ShouldSkipPath(rootAbs, path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if workspace.ShouldSkipName(name) {
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
		depth := workspace.PathDepth(relFromTarget)
		if depth > l.MaxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relFromRoot, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return nil
		}
		rel := filepath.ToSlash(relFromRoot)
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		entry := listFilesEntry{
			rel:     rel,
			abs:     abs,
			display: rel,
			mime:    assets.MIMEType(rel),
		}
		entry.kind = assets.KindForMIME(entry.mime)
		if d.IsDir() {
			entry.kind = "directory"
			entry.display = rel + "/"
			entry.mime = ""
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return tools.ToolResult{}, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].display < entries[j].display
	})
	output, assetRefs, err := l.assetResult(rootAbs, ec, targetRel, targetAbs, entries)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if len(entries) == 0 {
		return tools.ToolResult{Content: "(empty)", Output: output, Assets: assetRefs}, nil
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.display)
	}
	return tools.ToolResult{
		Content: strings.Join(lines, "\n"),
		Output:  output,
		Assets:  assetRefs,
	}, nil
}

func (l *ListFilesTool) assetResult(rootAbs string, ec tools.ExecutionContext, rel, abs string, entries []listFilesEntry) (json.RawMessage, []assets.Ref, error) {
	workspaceID := assets.WorkspaceID(rootAbs)
	items := make([]listFilesOutputItem, 0, len(entries))
	refs := make([]assets.Ref, 0, len(entries))
	for i, entry := range entries {
		id := assets.StableID(ec.TurnID, ec.CallID, i+1, "list_files", entry.kind, entry.rel)
		items = append(items, listFilesOutputItem{
			AssetID:      id,
			Kind:         entry.kind,
			Path:         entry.rel,
			AbsolutePath: entry.abs,
			DisplayName:  entry.display,
			MIMEType:     entry.mime,
		})
		refs = append(refs, assets.Ref{
			ID:                    id,
			Kind:                  entry.kind,
			URI:                   assets.WorkspaceURI(workspaceID, entry.rel, nil),
			DisplayName:           entry.display,
			WorkspaceID:           workspaceID,
			WorkspaceRelativePath: entry.rel,
			AbsolutePath:          entry.abs,
			MIMEType:              entry.mime,
			SourceTurnID:          ec.TurnID,
			SourceCallID:          ec.CallID,
		})
	}
	output, err := tools.JSONOutput(listFilesOutput{
		Kind:         "directory_listing",
		Path:         rel,
		AbsolutePath: abs,
		Items:        items,
	})
	if err != nil {
		return nil, nil, err
	}
	return output, refs, nil
}
