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
	MaxBytes int64
}

type editFileInput struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

func NewEditFileTool() *EditFileTool {
	return &EditFileTool{
		MaxBytes: 200_000,
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

func (t *EditFileTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
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
		if !ec.PathAccessApprover.ApproveExternalPath(targetAbs, "write") {
			return tools.ToolResult{Content: fmt.Sprintf("Access to external path was denied: %s", in.Path)}, nil
		}
		// approved — fall through to execute normally
	case workspace.PathInsideWorkspace:
		// proceed
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

	m := locateEdit(content, in.Old)
	if m.ambiguous > 0 {
		return tools.ToolResult{Content: fmt.Sprintf(
			"The 'old' text appears %d times in %s, so the edit is ambiguous. Include more surrounding context so it matches exactly once.", m.ambiguous, in.Path)}, nil
	}
	if !m.found {
		msg := fmt.Sprintf(
			"Could not find the 'old' text in %s. It must match the file exactly, including whitespace and indentation.", in.Path)
		if nm := nearMissDiagnostic(content, in.Old); nm != "" {
			msg += "\n\n" + nm
		} else {
			msg += " Re-read the file and copy the text verbatim."
		}
		return tools.ToolResult{Content: msg}, nil
	}

	newContent := content[:m.start] + in.New + content[m.end:]

	if err := os.WriteFile(targetAbs, []byte(newContent), info.Mode().Perm()); err != nil {
		return tools.ToolResult{}, err
	}

	lineNum := lineAt(content, m.start) + 1 // 1-based
	diff := editDiff(content, newContent, m.matched, in.New, m.start, 3)
	header := fmt.Sprintf("Edited %s at line %d", in.Path, lineNum)
	if m.tolerant {
		header += fmt.Sprintf(" (matched %s — verify the result)", m.note)
	}
	return tools.ToolResult{
		Content: fmt.Sprintf("%s:\n%s", header, diff),
	}, nil
}

// matchResult is where in content an edit should be applied. start/end is the
// byte range to replace; matched is content[start:end] (which for a tolerant
// match differs from the model's 'old'). tolerant/note record that a non-exact
// tier was used so the caller can flag it. ambiguous>0 means the text matched
// several places and the edit was refused.
type matchResult struct {
	found     bool
	start     int
	end       int
	matched   string
	tolerant  bool
	note      string
	ambiguous int
}

// locateEdit finds where to apply an edit, from strictest to most forgiving:
//  1. exact substring match;
//  2. after stripping read_file's "123\t" line-number prefixes, which the model
//     frequently leaves on the 'old' text despite the warning;
//  3. after folding typographic look-alikes (smart quotes, dashes, exotic
//     spaces) to ASCII — the characters an LLM most often fails to reproduce
//     byte-for-byte, especially in CJK text.
//
// A tier only wins on a unique match; multiple matches short-circuit to
// ambiguous so a forgiving tier never guesses. Tolerant matches apply the
// replacement at the real byte span, preserving the file's actual characters
// outside the edit.
func locateEdit(content, old string) matchResult {
	// Tier 1: exact.
	switch n := strings.Count(content, old); {
	case n == 1:
		i := strings.Index(content, old)
		return matchResult{found: true, start: i, end: i + len(old), matched: old}
	case n > 1:
		return matchResult{ambiguous: n}
	}

	// Tier 2: the model left read_file's line-number prefixes on 'old'.
	if stripped, changed := stripLineNumberPrefixes(old); changed {
		switch n := strings.Count(content, stripped); {
		case n == 1:
			i := strings.Index(content, stripped)
			return matchResult{found: true, start: i, end: i + len(stripped), matched: stripped,
				tolerant: true, note: "after stripping read_file's line-number prefixes"}
		case n > 1:
			return matchResult{ambiguous: n}
		}
	}

	// Tier 3: typographic fold (smart quotes / dashes / non-breaking spaces).
	normContent, offsets := foldWithMap(content)
	normOld := foldString(old)
	switch n := strings.Count(normContent, normOld); {
	case n == 1:
		ns := strings.Index(normContent, normOld)
		start, end := offsets[ns], offsets[ns+len(normOld)]
		return matchResult{found: true, start: start, end: end, matched: content[start:end],
			tolerant: true, note: "after folding smart quotes / dashes / non-breaking spaces to ASCII"}
	case n > 1:
		return matchResult{ambiguous: n}
	}

	return matchResult{found: false}
}

// foldRune maps typographic look-alikes to their ASCII equivalent and leaves
// every other rune untouched. Tabs and regular spaces are preserved so real
// indentation still has to match.
func foldRune(r rune) rune {
	switch r {
	case '“', '”', '„', '‟', '″', '〃': // “ ” „ ‟ ″ 〃
		return '"'
	case '‘', '’', '‚', '‛', '′': // ‘ ’ ‚ ‛ ′
		return '\''
	case '‐', '‑', '‒', '–', '—', '―', '−': // ‐ ‑ ‒ – — ― −
		return '-'
	case ' ', ' ', ' ', ' ', ' ', ' ', ' ', '　': // various spaces
		return ' '
	}
	return r
}

func foldString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

// foldWithMap folds s like foldString but also returns offsets, mapping each
// byte position in the folded string back to the byte offset of the originating
// rune in s (with a final sentinel of len(s)). It lets a match found in folded
// space be applied to the real bytes of s.
func foldWithMap(s string) (string, []int) {
	var b strings.Builder
	b.Grow(len(s))
	offsets := make([]int, 0, len(s)+1)
	for i, r := range s {
		fr := foldRune(r)
		n := utf8.RuneLen(fr)
		if n < 0 {
			n = 1
		}
		for j := 0; j < n; j++ {
			offsets = append(offsets, i)
		}
		b.WriteRune(fr)
	}
	offsets = append(offsets, len(s))
	return b.String(), offsets
}

// stripLineNumberPrefixes removes a leading "<spaces><digits>\t" from every line
// — the prefix read_file prints. changed is true only when every non-empty line
// carried such a prefix, so code that merely starts with a number is left alone.
func stripLineNumberPrefixes(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	any := false
	for _, l := range lines {
		if l == "" {
			continue
		}
		if lineNumberPrefixLen(l) == 0 {
			return s, false
		}
		any = true
	}
	if !any {
		return s, false
	}
	for i, l := range lines {
		lines[i] = l[lineNumberPrefixLen(l):]
	}
	return strings.Join(lines, "\n"), true
}

// lineNumberPrefixLen returns the byte length of a leading "<spaces><digits>\t"
// prefix, or 0 if the line does not start with one.
func lineNumberPrefixLen(l string) int {
	i := 0
	for i < len(l) && l[i] == ' ' {
		i++
	}
	d := i
	for i < len(l) && l[i] >= '0' && l[i] <= '9' {
		i++
	}
	if i == d {
		return 0 // no digits
	}
	if i < len(l) && l[i] == '\t' {
		return i + 1
	}
	return 0
}

// nearMissDiagnostic helps the model recover after all match tiers fail. It
// anchors on the first non-blank line of 'old', finds the file line that shares
// the longest folded prefix with it, and reports the first rune that differs —
// naming both codepoints so a smart-quote or whitespace mismatch is obvious.
// Returns "" when no line is similar enough to point at confidently.
func nearMissDiagnostic(content, old string) string {
	anchor := firstNonBlankLine(old)
	foldedAnchor := foldString(strings.TrimSpace(anchor))
	if len([]rune(foldedAnchor)) < 4 {
		return "" // too short to anchor on without false positives
	}

	fileLines := strings.Split(content, "\n")
	best, bestScore := -1, 0
	for i, fl := range fileLines {
		if s := commonPrefixRunes(foldString(strings.TrimSpace(fl)), foldedAnchor); s > bestScore {
			best, bestScore = i, s
		}
	}
	if best < 0 || bestScore < 4 {
		return ""
	}

	fileLine := fileLines[best]
	var b strings.Builder
	fmt.Fprintf(&b, "The closest line in the file is line %d:\n    %s\nbut your 'old' text has:\n    %s",
		best+1, fileLine, anchor)
	if col, fr, br, differ := firstRuneDiff(fileLine, anchor); differ {
		fmt.Fprintf(&b, "\nFirst difference at column %d: file has %s, your text has %s.", col, describeRune(fr), describeRune(br))
		b.WriteString("\nCopy the text verbatim from read_file — do not substitute smart quotes, dashes, or spaces.")
	} else {
		b.WriteString("\nThe first line matches; the mismatch is on a later line or in the surrounding whitespace. Re-copy the whole block verbatim.")
	}
	return b.String()
}

func firstNonBlankLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

func commonPrefixRunes(a, b string) int {
	ar, br := []rune(a), []rune(b)
	n := 0
	for n < len(ar) && n < len(br) && ar[n] == br[n] {
		n++
	}
	return n
}

// firstRuneDiff returns the 1-based column of the first differing rune between a
// and b and both runes there; a rune of -1 means that side ended first.
func firstRuneDiff(a, b string) (col int, aRune, bRune rune, differ bool) {
	ar, br := []rune(a), []rune(b)
	i := 0
	for i < len(ar) && i < len(br) {
		if ar[i] != br[i] {
			return i + 1, ar[i], br[i], true
		}
		i++
	}
	if i < len(ar) {
		return i + 1, ar[i], -1, true
	}
	if i < len(br) {
		return i + 1, -1, br[i], true
	}
	return 0, 0, 0, false
}

func describeRune(r rune) string {
	switch {
	case r < 0:
		return "nothing (end of line)"
	case r == ' ':
		return "a space (U+0020)"
	case r == '\t':
		return "a tab (U+0009)"
	default:
		return fmt.Sprintf("%q (U+%04X)", r, r)
	}
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
