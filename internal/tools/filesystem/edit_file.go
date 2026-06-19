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

type EditFileTool struct {
	WorkspaceRoot string
	MaxBytes      int64
}

type editFileInput struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

func NewEditFileTool(workspace string) *EditFileTool {
	return &EditFileTool{
		WorkspaceRoot: workspace,
		MaxBytes:      200_000,
	}
}

func (t *EditFileTool) Name() string {
	return "edit_file"
}

func (t *EditFileTool) Description() string {
	return "Replace an exact, unique span of text in a file. Provide 'old' (the text to find, copied verbatim from the file including indentation) and 'new' (the replacement; leave empty to delete). The 'old' text must occur exactly once — include enough surrounding context to make it unique. IMPORTANT: strip the line-number prefix (e.g. '142\\t') that read_file adds to each line — the actual file does not contain these prefixes, so including them will cause 'Could not find the old text'."
}

func (t *EditFileTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"path": {
			Type:        "string",
			Description: "File path relative to the workspace root.",
		},
		"old": {
			Type:        "string",
			Description: "The exact text to replace, copied verbatim from the file (including whitespace). Must match exactly once.",
		},
		"new": {
			Type:        "string",
			Description: "The replacement text. Leave empty to delete the matched text.",
		},
	}, "path", "old").JSON()
}

func (t *EditFileTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	var in editFileInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid edit_file input: %w", err)
		}
	}
	if in.Path == "" {
		return tools.ToolResult{}, fmt.Errorf("edit_file input: path is required")
	}

	select {
	case <-ctx.Done():
		return tools.ToolResult{}, ctx.Err()
	default:
	}

	// Recoverable conditions are returned as observations (nil error) so the
	// model can adjust its arguments and retry, rather than aborting.
	if in.Old == "" {
		return tools.ToolResult{Content: "The 'old' text is empty. Provide the exact text to replace."}, nil
	}
	if in.Old == in.New {
		return tools.ToolResult{Content: "The 'old' and 'new' text are identical; nothing to change."}, nil
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
	if !workspace.IsSubPath(rootAbs, targetAbs) {
		return tools.ToolResult{}, fmt.Errorf("path escapes workspace: %s", in.Path)
	}

	info, err := os.Stat(targetAbs)
	if err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("Could not open %s: %v", in.Path, err)}, nil
	}
	if info.IsDir() {
		return tools.ToolResult{Content: fmt.Sprintf("%s is a directory, not a file.", in.Path)}, nil
	}
	if info.Size() > t.MaxBytes {
		return tools.ToolResult{Content: fmt.Sprintf("File too large to edit: %s (size=%d, max=%d).", in.Path, info.Size(), t.MaxBytes)}, nil
	}

	data, err := os.ReadFile(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if !utf8.Valid(data) {
		return tools.ToolResult{Content: fmt.Sprintf("%s is not a UTF-8 text file.", in.Path)}, nil
	}

	// Match against LF-normalized content. read_file hands the model
	// LF-normalized text, so the 'old' string it quotes back is LF; matching
	// against raw CRLF would fail. The file is rewritten as LF.
	content := strings.ReplaceAll(string(data), "\r\n", "\n")

	switch strings.Count(content, in.Old) {
	case 0:
		return tools.ToolResult{Content: fmt.Sprintf(
			"Could not find the 'old' text in %s. It must match the file exactly, including whitespace and indentation. Re-read the file and copy the text verbatim.", in.Path)}, nil
	case 1:
		// unique match — proceed
	default:
		n := strings.Count(content, in.Old)
		return tools.ToolResult{Content: fmt.Sprintf(
			"The 'old' text appears %d times in %s, so the edit is ambiguous. Include more surrounding context so it matches exactly once.", n, in.Path)}, nil
	}

	idx := strings.Index(content, in.Old)
	newContent := content[:idx] + in.New + content[idx+len(in.Old):]

	if err := os.WriteFile(targetAbs, []byte(newContent), info.Mode().Perm()); err != nil {
		return tools.ToolResult{}, err
	}

	lineNum := lineAt(content, idx) + 1 // 1-based
	diff := editDiff(content, newContent, in.Old, in.New, idx, 3)
	return tools.ToolResult{
		Content: fmt.Sprintf("Edited %s at line %d:\n%s", in.Path, lineNum, diff),
	}, nil
}

// editDiff renders a unified-diff-like view of an edit: context before (plain),
// old text (-), new text (+), and context after (plain, from the new content so
// line numbers are correct). Each line carries a 1-based line number — the reader
// can see what was there, what it became, and what surrounds it.
func editDiff(oldContent, newContent, oldStr, newStr string, oldStart, ctx int) string {
	oldLines := strings.Split(oldContent, "\n")
	newContLines := strings.Split(newContent, "\n")

	oldLine := lineAt(oldContent, oldStart)            // 0-based line index of the old text start
	oldEnd := lineAt(oldContent, oldStart+len(oldStr)) // 0-based line index of old text end

	newStrLines := strings.Split(newStr, "\n")
	added := len(newStrLines)       // lines in the new text
	removed := oldEnd - oldLine + 1 // lines removed
	lineShift := added - removed    // how much later lines shift

	from := oldLine - ctx
	if from < 0 {
		from = 0
	}
	toOld := oldEnd + ctx
	if toOld > len(oldLines)-1 {
		toOld = len(oldLines) - 1
	}

	var b strings.Builder

	// Context before (from old content — these lines are unchanged).
	for i := from; i < oldLine; i++ {
		_, _ = fmt.Fprintf(&b, " %d\t%s\n", i+1, oldLines[i])
	}

	// Old lines (removed, from old content).
	for i := oldLine; i <= oldEnd; i++ {
		_, _ = fmt.Fprintf(&b, "-%d\t%s\n", i+1, oldLines[i])
	}

	// New lines (added).
	for j, nl := range newStrLines {
		_, _ = fmt.Fprintf(&b, "+%d\t%s\n", oldLine+1+j, nl)
	}

	// Context after (from new content, with corrected line numbers).
	newAfter := oldEnd + lineShift + 1 // first unchanged line after the edit in newContent
	toNew := oldEnd + lineShift + ctx
	if toNew > len(newContLines)-1 {
		toNew = len(newContLines) - 1
	}
	for i := newAfter; i <= toNew; i++ {
		_, _ = fmt.Fprintf(&b, " %d\t%s\n", i+1, newContLines[i])
	}

	return strings.TrimRight(b.String(), "\n")
}

// lineAt returns the 0-based index of the line containing byteOffset.
func lineAt(content string, byteOffset int) int {
	if byteOffset > len(content) {
		byteOffset = len(content)
	}
	return strings.Count(content[:byteOffset], "\n")
}

// SideEffects marks edit_file as a mutating tool; the runtime gates it behind
// user confirmation.
func (t *EditFileTool) SideEffects() bool { return true }

var (
	_ tools.Tool          = (*EditFileTool)(nil)
	_ tools.SideEffecting = (*EditFileTool)(nil)
)
