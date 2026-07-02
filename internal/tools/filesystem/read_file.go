package filesystem

import (
	"bufio"
	"code-agent/internal/assetref"
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
	MaxBytes int64
}

// lineNumber accepts both a JSON number (42) and a JSON string containing a
// number ("42"). Providers sometimes pass numeric fields as quoted strings,
// which would otherwise fail to unmarshal into an int.
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
	n, err := strconv.Atoi(strings.TrimSpace(s))
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

type readLine struct {
	no   int
	text string
}

type readFileOutput struct {
	Kind         string        `json:"kind"`
	Path         string        `json:"path"`
	AbsolutePath string        `json:"absolute_path,omitempty"`
	LineCount    int           `json:"line_count"`
	DisplayRange *assets.Range `json:"display_range,omitempty"`
	AssetID      string        `json:"asset_id"`
}

func NewReadFileTool() *ReadFileTool {
	return &ReadFileTool{
		MaxBytes: 200_000,
	}
}

func (r *ReadFileTool) Name() string {
	return "read_file"
}

func (r *ReadFileTool) Description() string {
	return "Read a UTF-8 text file from the workspace. Each line is prefixed with its 1-based line " +
		"number and a tab — these are for reference only and are NOT part of the file; do not include " +
		"them when editing. Use offset (1-based start line) and limit (line count) to read a window — " +
		"this works even on files too large to read whole, so prefer it for big files."
}

func (r *ReadFileTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"path": {
			Type:        "string",
			Description: "File path relative to the workspace root.",
		},
		"offset": {
			Type:        "integer",
			Description: "Optional. 1-based line number to start reading from. Default 1. Accepts a string number too.",
		},
		"limit": {
			Type:        "integer",
			Description: "Optional. Maximum number of lines to read from offset. Default reads to end of file. Accepts a string number too.",
		},
	}, "path").JSON()
}

func (r *ReadFileTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in readFileInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid read_file input: %w", err)
		}
	}
	if in.Path == "" {
		return tools.ToolResult{}, fmt.Errorf("invalid read_file input: path is required")
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return tools.ToolResult{}, ctx.Err()
		default:
		}
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}
	targetAbs, err := filepath.Abs(filepath.Join(rootAbs, filepath.Clean(in.Path)))
	if err != nil {
		return tools.ToolResult{}, err
	}
	if !workspace.IsSubPath(rootAbs, targetAbs) {
		return tools.ToolResult{}, fmt.Errorf("path escapes workspace: %s", in.Path)
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		rel = filepath.Clean(in.Path)
	}
	rel = filepath.ToSlash(rel)

	info, err := os.Stat(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if info.IsDir() {
		return tools.ToolResult{}, fmt.Errorf("path is a directory: %s", in.Path)
	}

	startLine := int(in.Offset)
	if startLine < 1 {
		startLine = 1
	}
	limit := int(in.Limit)
	windowed := startLine > 1 || limit > 0

	// A full read of a file larger than the budget is rejected, with a hint to
	// read a window instead. A *windowed* read streams and is bounded by the
	// window, so it works on files of any size.
	if !windowed && info.Size() > r.MaxBytes {
		return tools.ToolResult{}, fmt.Errorf("file too large to read whole: %s size=%d max=%d — read a window with offset/limit",
			in.Path, info.Size(), r.MaxBytes)
	}

	f, err := os.Open(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	defer f.Close()

	var collected []readLine
	var bytesOut int64
	truncated := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long lines
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo < startLine {
			continue
		}
		if limit > 0 && len(collected) >= limit {
			break
		}
		text := scanner.Text()
		if !utf8.ValidString(text) {
			return tools.ToolResult{}, fmt.Errorf("file is not valid UTF-8 text: %s", in.Path)
		}
		// Bound a windowed read's output so a huge limit (or long lines) cannot
		// blow the budget; surface that it was cut.
		bytesOut += int64(len(text)) + 1
		if windowed && bytesOut > r.MaxBytes {
			truncated = true
			break
		}
		collected = append(collected, readLine{lineNo, text})
	}
	if err := scanner.Err(); err != nil {
		return tools.ToolResult{}, err
	}

	displayRange := displayRangeFor(collected)
	output, assetRefs, err := r.assetResult(rootAbs, ec, rel, targetAbs, len(collected), displayRange, collected)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if len(collected) == 0 {
		return tools.ToolResult{Content: "", Output: output, Assets: assetRefs}, nil // offset past EOF, or empty file
	}

	// Right-align the line numbers to the width of the largest one shown.
	width := len(strconv.Itoa(collected[len(collected)-1].no))
	var b strings.Builder
	for _, l := range collected {
		fmt.Fprintf(&b, "%*d\t%s\n", width, l.no, l.text)
	}
	out := strings.TrimRight(b.String(), "\n")
	if truncated {
		out += "\n... (output truncated; read fewer lines with limit, or continue with a higher offset)"
	}
	return tools.ToolResult{Content: out, Output: output, Assets: assetRefs}, nil
}

func displayRangeFor(lines []readLine) *assets.Range {
	if len(lines) == 0 {
		return nil
	}
	return &assets.Range{
		StartLine: lines[0].no,
		EndLine:   lines[len(lines)-1].no,
	}
}

func (r *ReadFileTool) assetResult(rootAbs string, ec tools.ExecutionContext, rel, abs string, lineCount int, displayRange *assets.Range, collected []readLine) (json.RawMessage, []assets.Ref, error) {
	workspaceID := assets.WorkspaceID(rootAbs)
	id := assets.StableID(ec.TurnID, ec.CallID, 1, "read_file", rel, fmt.Sprint(displayRange))
	preview := ""
	if len(collected) > 0 {
		preview = strings.TrimSpace(collected[0].text)
	}
	output, err := tools.JSONOutput(readFileOutput{
		Kind:         "file",
		Path:         rel,
		AbsolutePath: abs,
		LineCount:    lineCount,
		DisplayRange: displayRange,
		AssetID:      id,
	})
	if err != nil {
		return nil, nil, err
	}
	ref := assets.Ref{
		ID:                    id,
		Kind:                  "file",
		URI:                   assets.WorkspaceURI(workspaceID, rel, displayRange),
		DisplayName:           assets.DisplayName(rel, 0),
		WorkspaceID:           workspaceID,
		WorkspaceRelativePath: rel,
		AbsolutePath:          abs,
		Range:                 displayRange,
		Preview:               preview,
		MIMEType:              assets.MIMEType(rel),
		SourceTurnID:          ec.TurnID,
		SourceCallID:          ec.CallID,
	}
	return output, []assets.Ref{ref}, nil
}
