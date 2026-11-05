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

type EditFileTool struct {
	MaxBytes int64
}

type editFileInput struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
	// AllowDelete acknowledges that an empty 'new' really means "delete this
	// text". Without it, emptying a non-blank span is refused: an empty 'new'
	// is the single most common shape of a truncated tool call (a replacement
	// that never made it into the arguments), and applying it destroys content
	// while reporting success.
	AllowDelete bool `json:"allow_delete"`
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
	return "Replace an exact, unique span of text in a file. Provide 'old' (the text to find, copied verbatim from the file including indentation) and 'new' (the replacement; leave empty to delete). The 'old' text must occur exactly once — include enough surrounding context to make it unique. To delete, 'new' must be empty AND allow_delete must be true. IMPORTANT: strip the line-number prefix (e.g. '142\\t') that read_file adds to each line — the actual file does not contain these prefixes, so including them will cause 'Could not find the old text'."
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
			Description: "The replacement text. Leave empty to delete the matched text — that requires allow_delete: true.",
		},
		"allow_delete": {
			Type:        "boolean",
			Description: "Set true only when 'new' is intentionally empty and the matched text should be deleted.",
		},
	}, "path", "old", "new").Closed().JSON()
}

func (t *EditFileTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	if err := tools.ValidateInput(t, input); err != nil {
		return tools.ToolResult{}, err
	}
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
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "invalid_arguments"}, Content: "The 'old' text is empty. Provide the exact text to replace."}, nil
	}
	if in.Old == in.New {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "invalid_arguments"}, Content: "The 'old' and 'new' text are identical; nothing to change."}, nil
	}

	targetAbs, denied, err := resolveWriteTarget(ec, in.Path)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if denied {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "blocked"}, Content: fmt.Sprintf("Access to external path was denied: %s", in.Path)}, nil
	}

	// Plan mode may revise its authoritative markdown plan, but it must not use
	// edit_file to mutate project files before propose_plan approval.
	if err := checkPlanModeTarget(ec, targetAbs, in.Path, "edit"); err != nil {
		return tools.ToolResult{}, err
	}

	info, err := os.Stat(targetAbs)
	if err != nil {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "not_found"}, Content: fmt.Sprintf("Could not open %s: %v", in.Path, err)}, nil
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

	// Match the read_file LF view, but keep untouched bytes and line endings.
	content, offsets := editLFView(string(data))
	in.Old = strings.ReplaceAll(in.Old, "\r\n", "\n")
	in.New = strings.ReplaceAll(in.New, "\r\n", "\n")

	m := locateEdit(content, in.Old)
	if m.ambiguous > 0 {
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "edit_conflict"}, Content: fmt.Sprintf(
			"The 'old' text appears %d times in %s, so the edit is ambiguous. Include more surrounding context so it matches exactly once.", m.ambiguous, in.Path)}, nil
	}
	if !m.found {
		msg := fmt.Sprintf(
			"Could not find the 'old' text in %s. It must match the file exactly, including whitespace and indentation.", in.Path)
		if nm := editDiagnostic(ec, input, content, in.Old, in.New); nm != "" {
			msg += "\n\n" + nm
		} else {
			msg += " Re-read the file and copy the text verbatim."
		}
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "edit_conflict"}, Content: msg}, nil
	}

	// An empty 'new' deletes the matched text. That is a legitimate edit, but it
	// is also exactly what a truncated tool call looks like: the replacement
	// never arrives and the arguments simply end after 'old'. Refuse unless the
	// model says it means to delete — otherwise content disappears while the
	// tool reports success and the model never retries.
	//
	// Strictly empty, not whitespace-only: a replacement of "\n" is how a caller
	// clears a line's text while keeping the line, and that is a normal edit.
	deleting := in.New == ""
	if deleting && strings.TrimSpace(m.matched) != "" && !in.AllowDelete {
		lineNum := lineAt(content, m.start) + 1
		return tools.ToolResult{Failure: &tools.ToolFailure{Kind: "blocked"}, Content: fmt.Sprintf(
			"Refused: 'new' is empty, which would delete %d line(s) starting at line %d of %s and put nothing in their place.\n"+
				"If you meant to delete this text, re-send the same call with \"allow_delete\": true.\n"+
				"If you meant to replace it, put the replacement in 'new' — an empty 'new' usually means the replacement text was dropped from the tool call. Nothing was changed.",
			len(wholeLinesOf(m.matched)), lineNum, in.Path)}, nil
	}

	newContent := content[:m.start] + in.New + content[m.end:]
	if newContent == content {
		return tools.FailureResult("invalid_arguments", "The replacement makes no change to the file."), nil
	}
	replacement := in.New
	if strings.Contains(string(data), "\r\n") && !strings.Contains(strings.ReplaceAll(string(data), "\r\n", ""), "\n") {
		replacement = strings.ReplaceAll(replacement, "\n", "\r\n")
	}
	written := string(data[:offsets[m.start]]) + replacement + string(data[offsets[m.end]:])
	if err := os.WriteFile(targetAbs, []byte(written), info.Mode().Perm()); err != nil {
		return tools.ToolResult{}, err
	}

	lineNum := lineAt(content, m.start) + 1 // 1-based
	diff := editDiff(content, newContent, m.matched, in.New, m.start, 3)
	header := fmt.Sprintf("Edited %s at line %d", in.Path, lineNum)
	if deleting {
		header = fmt.Sprintf("Deleted %d line(s) at line %d of %s", len(wholeLinesOf(m.matched)), lineNum, in.Path)
	}
	if m.tolerant {
		header += fmt.Sprintf(" (matched %s — verify the result)", m.note)
	}
	return tools.ToolResult{
		Content: fmt.Sprintf("%s:\n%s", header, diff),
		// Reported only now, after the write above returned: this is what reached
		// disk, not what the call intended. Counts use the text actually replaced
		// (m.matched, which a tolerant match may differ from 'old' on) and the
		// replacement the model supplied. allow_delete deletes text inside the
		// file, so the status stays modified — a whole-file delete is only
		// apply_patch's.
		Wrote: []tools.FileWrite{{
			Path:    diffPath(ec, targetAbs),
			Added:   tools.LineCount(in.New),
			Removed: tools.LineCount(m.matched),
			Status:  tools.WriteModified,
		}},
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
		best+1, boundedEditLine(fileLine), boundedEditLine(anchor))
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
//
// Line accounting is on WHOLE touched lines. A span that ends with "\n" stops at
// the end of its own line: the newline terminates that line, it does not start
// the next one, so the following line is context, not part of the edit. Getting
// this wrong makes a successful edit read as "one extra line removed, one blank
// line added", which is indistinguishable from data loss — the model then
// "restores" content that was never lost.
func editDiff(oldContent, newContent, oldStr, newStr string, oldStart, ctx int) string {
	oldLines := strings.Split(oldContent, "\n")
	newContLines := strings.Split(newContent, "\n")

	// oldStr is non-empty for edit_file, so the last touched byte is at
	// oldStart+len(oldStr)-1: when oldStr ends with "\n" that byte IS the
	// terminator, which still belongs to the span's last line. An empty oldStr
	// is a pure insertion: it removes nothing, so the span is empty.
	firstLine := lineAt(oldContent, oldStart)
	lastLine := firstLine - 1
	if len(oldStr) > 0 {
		lastLine = lineAt(oldContent, oldStart+len(oldStr)-1)
		if lastLine < firstLine {
			lastLine = firstLine
		}
	}

	// Whole lines added, excluding the trailing "" produced by a terminating
	// newline (and the degenerate "" of an empty replacement).
	newStrLines := splitWholeLines(newStr)
	added := len(newStrLines)
	removed := lastLine - firstLine + 1
	if removed < 0 {
		removed = 0
	}
	lineShift := added - removed

	from := firstLine - ctx
	if from < 0 {
		from = 0
	}
	toOld := lastLine + ctx
	if toOld > len(oldLines)-1 {
		toOld = len(oldLines) - 1
	}

	var b strings.Builder

	// Context before (from old content — these lines are unchanged).
	for i := from; i < firstLine; i++ {
		_, _ = fmt.Fprintf(&b, " %d\t%s\n", i+1, oldLines[i])
	}

	// Old lines (removed, from old content).
	for i := firstLine; i <= lastLine; i++ {
		_, _ = fmt.Fprintf(&b, "-%d\t%s\n", i+1, oldLines[i])
	}

	// New lines (added).
	for j, nl := range newStrLines {
		_, _ = fmt.Fprintf(&b, "+%d\t%s\n", firstLine+1+j, nl)
	}

	// Context after (from new content, with corrected line numbers).
	newAfter := lastLine + 1 + lineShift // first unchanged line after the edit in newContent
	toNew := newAfter + ctx - 1
	if toNew > len(newContLines)-1 {
		toNew = len(newContLines) - 1
	}
	for i := newAfter; i <= toNew; i++ {
		_, _ = fmt.Fprintf(&b, " %d\t%s\n", i+1, newContLines[i])
	}

	return strings.TrimRight(b.String(), "\n")
}

// splitWholeLines splits a replacement string into the lines it actually adds.
// "" adds nothing, and a trailing "\n" terminates the last line instead of
// adding an empty one — otherwise every edit ending in a newline reports a
// phantom blank line at the end.
func splitWholeLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// wholeLinesOf is splitWholeLines for a span that is known to be non-empty: it
// always reports at least one line, so a deletion is never described as
// affecting zero lines.
func wholeLinesOf(s string) []string {
	if lines := splitWholeLines(s); len(lines) > 0 {
		return lines
	}
	return []string{s}
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
