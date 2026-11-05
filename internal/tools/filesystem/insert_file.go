package filesystem

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// InsertFileTool adds whole lines to an existing file at a line address.
//
// It exists because both existing writers require the model to reproduce source
// text byte-for-byte before anything can change: edit_file needs an exact 'old'
// span, apply_patch needs exact context lines. For a long CJK paragraph — where
// one look-alike character, a smart quote or a dropped backtick breaks the match
// — that verification step, not the new text, is the fragile part. Addressing by
// line number removes the match entirely.
//
// Scope is deliberately narrow: one file, one insertion point, add-only. It does
// not replace, delete, or touch more than one file, so its blast radius is
// statically knowable from (path, line) — unlike apply_patch, whose single
// 'patch' string can rewrite many files.
type InsertFileTool struct {
	MaxBytes int64
}

type insertFileInput struct {
	Path string `json:"path"`
	// Line is 1-based: content goes after that line. 0 inserts at the top of the
	// file. Same numbering read_file prints.
	Line    lineNumber `json:"line"`
	Content string     `json:"content"`
}

func NewInsertFileTool() *InsertFileTool {
	return &InsertFileTool{MaxBytes: 200_000}
}

func (t *InsertFileTool) Name() string { return "insert_file" }

func (t *InsertFileTool) Description() string {
	return "Insert new lines into an existing file at a line number. 'line' is the number read_file shows; the content goes AFTER that line, and line 0 inserts at the top of the file. " +
		"Use this when the change only ADDS text: unlike edit_file there is no 'old' text to match, so long paragraphs, CJK text, and lines full of quotes or backticks cannot fail to match. " +
		"Content is inserted as whole lines (a trailing newline is added if missing). Nothing is replaced or deleted — use edit_file for that."
}

func (t *InsertFileTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"path": {
			Type:        "string",
			Description: "File path relative to the workspace root.",
		},
		"line": {
			Type:        "integer",
			Description: "1-based line number to insert AFTER. Use 0 to insert at the top of the file.",
		},
		"content": {
			Type:        "string",
			Description: "The lines to insert. Must be non-empty.",
		},
	}, "path", "content").JSON()
}

func (t *InsertFileTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in insertFileInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid insert_file input: %w", err)
		}
	}
	if in.Path == "" {
		return tools.ToolResult{}, fmt.Errorf("insert_file input: path is required")
	}
	if in.Content == "" {
		return tools.ToolResult{}, fmt.Errorf("insert_file input: content is required (use edit_file to delete or replace text)")
	}
	if in.Line < 0 {
		return tools.ToolResult{}, fmt.Errorf("insert_file input: line must be 0 or greater (0 inserts at the top)")
	}

	select {
	case <-ctx.Done():
		return tools.ToolResult{}, ctx.Err()
	default:
	}

	targetAbs, denied, err := resolveWriteTarget(ec, in.Path)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if denied {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "blocked"}, Content: fmt.Sprintf("Access to external path was denied: %s", in.Path)}, nil
	}

	// Plan mode may revise its authoritative markdown plan, but nothing else.
	if err := checkPlanModeTarget(ec, targetAbs, in.Path, "insert into"); err != nil {
		return tools.ToolResult{}, err
	}

	info, err := os.Stat(targetAbs)
	if err != nil {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "runtime"}, Content: fmt.Sprintf("Could not open %s: %v", in.Path, err)}, nil
	}
	if info.IsDir() {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "runtime"}, Content: fmt.Sprintf("%s is a directory, not a file.", in.Path)}, nil
	}
	if info.Size() > t.MaxBytes {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "runtime"}, Content: fmt.Sprintf("File too large to edit: %s (size=%d, max=%d).", in.Path, info.Size(), t.MaxBytes)}, nil
	}

	data, err := os.ReadFile(targetAbs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if !utf8.Valid(data) {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "runtime"}, Content: fmt.Sprintf("%s is not a UTF-8 text file.", in.Path)}, nil
	}

	// Same normalization edit_file applies: read_file hands the model LF text, so
	// the line numbers it prints describe the LF view of the file.
	content := strings.ReplaceAll(string(data), "\r\n", "\n")

	// A trailing newline terminates the last line rather than adding an empty one.
	insert := strings.ReplaceAll(in.Content, "\r\n", "\n")
	if !strings.HasSuffix(insert, "\n") {
		insert += "\n"
	}

	newContent, offset, insertedLines, err := insertWholeLines(content, int(in.Line), insert)
	if err != nil {
		total := countLines(content)
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "runtime"}, Content: fmt.Sprintf(
			"%v %s has %d line(s). Line 0 inserts at the top; line N inserts after line N, using the numbers read_file prints. Re-read the file to confirm the line number — nothing was changed.",
			err, in.Path, total)}, nil
	}
	if int64(len(newContent)) > t.MaxBytes {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "runtime"}, Content: fmt.Sprintf(
			"Refusing the insert: %s would grow to %d bytes (max %d).", in.Path, len(newContent), t.MaxBytes)}, nil
	}

	if err := os.WriteFile(targetAbs, []byte(newContent), info.Mode().Perm()); err != nil {
		return tools.ToolResult{}, err
	}

	header := fmt.Sprintf("Inserted %d line(s) after line %d of %s", insertedLines, int(in.Line), in.Path)
	if in.Line == 0 {
		header = fmt.Sprintf("Inserted %d line(s) at the top of %s", insertedLines, in.Path)
	}
	return tools.ToolResult{
		Content: fmt.Sprintf("%s:\n%s", header, editDiff(content, newContent, "", insert, offset, 1)),
		Wrote: []tools.FileWrite{{
			Path:   diffPath(ec, targetAbs),
			Added:  insertedLines,
			Status: tools.WriteModified,
		}},
	}, nil
}

// insertWholeLines inserts whole lines into content after the 1-based line
// `after` (0 = top of file) and returns the new content plus the byte offset in
// the OLD content where the insertion landed (so a diff can be rendered against
// it). Lines are counted the way read_file prints them; a file's trailing
// newline is preserved rather than normalized.
func insertWholeLines(content string, after int, insert string) (string, int, int, error) {
	var existing []string
	if content != "" {
		existing = strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	}
	if after > len(existing) {
		return "", 0, 0, fmt.Errorf("line %d is out of range.", after)
	}

	block := splitWholeLines(insert)
	merged := make([]string, 0, len(existing)+len(block))
	merged = append(merged, existing[:after]...)
	merged = append(merged, block...)
	merged = append(merged, existing[after:]...)

	out := strings.Join(merged, "\n")
	// An empty source file has no trailing newline to preserve; give the result
	// one so the inserted line is a line, not a fragment.
	if strings.HasSuffix(content, "\n") || content == "" {
		out += "\n"
	}

	offset := 0
	if after > 0 {
		offset = len(strings.Join(existing[:after], "\n")) + 1
	}
	return out, offset, len(block), nil
}

// countLines reports how many lines read_file would number for content. It
// delegates so the filesystem tools and the diffstat share one definition; the
// trailing-newline rule is the part that used to drift by one line.
func countLines(content string) int { return tools.LineCount(content) }

// SideEffects marks insert_file as a mutating tool; the runtime gates it behind
// user confirmation.
func (t *InsertFileTool) SideEffects() bool { return true }

var (
	_ tools.Tool          = (*InsertFileTool)(nil)
	_ tools.SideEffecting = (*InsertFileTool)(nil)
)
