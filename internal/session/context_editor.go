package session

import (
	"code-agent/internal/model"
	"fmt"
	"strings"
)

// ContextEditor prunes stale tool results from session history before
// compaction. It's a free (no LLM call) cleanup step that removes
// information the model no longer needs — old build output, spent command
// results, verbose listings.
//
// Unlike the original time-window-only strategy (clear everything older than
// N turns), this version uses content-value triage:
//   - Short or code-rich results (API surfaces, signatures, grep hits) are
//     preserved so the LLM Compactor can summarize them properly.
//   - Verbose, low-signal results (build output, directory listings) are
//     replaced with a structured skeleton that records what was run and how
//     much output it produced.
//
// This mirrors Claude Code's context editing strategy applied before the
// LLM summarizer runs: clear the noise, preserve the signal.
type ContextEditor struct {
	// KeepTurns is the number of most recent assistant turns to preserve
	// in full. A "turn" is one assistant message + its tool call/result
	// pairs. Default: 3.
	KeepTurns int
}

// toolCallInfo is the minimal metadata extracted from an assistant message's
// tool calls, used to build clearing skeletons without re-scanning.
type toolCallInfo struct {
	name  string // tool name (e.g. "read_file", "bash")
	input string // short input summary (e.g. "theme/theme.go", "go build ./...")
}

// Edit clears old tool results that carry low signal. It never removes
// messages — only their content is replaced with a placeholder marker or
// a structured skeleton. Errors and high-value results (API surfaces,
// signatures) are preserved. The message structure stays valid for
// resending to the provider.
//
// Edit is idempotent: calling it twice on the same session is a no-op
// after the first pass. Returns the number of messages edited.
func (e ContextEditor) Edit(sess *Session) int {
	keep := e.KeepTurns
	if keep <= 0 {
		keep = 3
	}
	msgs := sess.Messages
	cutoff := findCutoff(msgs, keep)
	if cutoff <= 0 {
		return 0
	}

	// Build a map of ToolCallID → tool metadata in one pass so we can
	// build informative skeletons without re-scanning per result.
	toolMap := buildToolCallMap(msgs)

	edited := 0
	for i := 0; i < cutoff; i++ {
		m := &msgs[i]
		if m.Role != model.RoleTool {
			continue
		}
		if m.Content == clearedMarker || strings.HasPrefix(m.Content, "[cleared:") {
			continue // already cleared (idempotent)
		}
		if isError(m.Content) {
			continue // diagnostic errors are always preserved
		}
		if isHighValue(m.Content, toolMap[m.ToolCallID]) {
			continue // API surfaces, code, grep hits — keep for LLM summary
		}

		// Low-signal result: replace with a structured skeleton so the
		// LLM Compactor knows what was here without paying for the bytes.
		m.Content = buildSkeleton(m.ToolCallID, m.Content, toolMap)
		edited++
	}
	return edited
}

// clearedMarker is the legacy bare placeholder. Results cleared by the
// new strategy use structured skeletons instead; this constant only
// serves as the idempotency sentinel and as a fallback.
const clearedMarker = "[tool result cleared]"

// findCutoff returns the index of the first message to KEEP. Messages before
// this index are candidates for pruning.
func findCutoff(msgs []model.Message, keepTurns int) int {
	turns := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant {
			turns++
			if turns >= keepTurns {
				return i
			}
		}
	}
	return 0
}

// isError reports whether a tool result should be preserved because it
// carries diagnostically useful information.
//
// Patterns match at line beginnings or with surrounding whitespace to avoid
// false positives from words like "failed" appearing in commit messages,
// code comments, or variable names (e.g. "// Fixed failed build" or
// "lastFailedAttempt := ...").
func isError(content string) bool {
	// Line-start patterns: "error:", "Error:", "ERROR:" at the beginning of a
	// line or with only whitespace/symbols before it.
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "error:") ||
			strings.HasPrefix(trimmed, "Error:") ||
			strings.HasPrefix(trimmed, "ERROR:") {
			return true
		}
	}

	// "exit status" followed by a digit (exit status 1, exit status 127).
	if strings.Contains(content, "exit status ") {
		// Only match when followed by a digit to avoid matching prose.
		idx := strings.Index(content, "exit status ")
		if idx >= 0 {
			rest := content[idx+len("exit status "):]
			if len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
				return true
			}
		}
	}

	// "failed" as a stand-alone line or surrounded by newlines/whitespace,
	// not embedded in a sentence like "Fixed failed build".
	if strings.Contains(content, "\nfailed") ||
		strings.Contains(content, "\nFailed") ||
		strings.Contains(content, "\nFAILED") ||
		strings.Contains(content, "\nfailure:") {
		return true
	}

	// P4.1 observation marker.
	if strings.Contains(content, "failure=") {
		return true
	}

	return false
}

// isHighValue reports whether a tool result carries enough signal to be
// worth preserving for the LLM Compactor, even though it falls outside
// the recent-turn window. info identifies the tool that produced this result
// and its target; when the call metadata is missing (compacted history),
// info.name is empty and the decision falls back to content-only heuristics.
//
// Tool-type awareness disambiguates cases where content patterns alone
// misclassify — e.g. JSON configs lack code markers but carry architectural
// constraints, and verbose stack traces match file:line patterns without
// carrying API surface information.
//
// Heuristic (deterministic, no LLM):
//   - Very short results (< 200B): keep only if code-like. A 14-byte
//     "normal output" is noise; a 22-byte "func main() { ... }" is an API surface.
//   - Medium results (200B-2KB): generally worth keeping — moderate
//     information density (function signatures, grep results, short file reads).
//   - Larger results (> 2KB): keep only if code-rich (type/function
//     declarations, file:line patterns) — verbose build output and
//     directory listings are low-signal.
func isHighValue(content string, info toolCallInfo) bool {
	// Tool-type awareness: the tool and its target disambiguate cases
	// where content patterns alone get it wrong.
	switch info.name {
	case "read_file":
		// File reads are intentional data gathering — the agent asked for
		// this specific file. Config, doc, and data formats carry
		// architectural decisions and constraints that code-marker
		// heuristics systematically miss.
		if isConfigOrDocExt(info.input) {
			return true
		}
		// For any file read, lower the bar: up to 4KB is worth keeping
		// for the LLM summarizer (covers CLI help text, small data files,
		// short scripts the agent explicitly requested).
		if len(content) <= 4096 {
			return true
		}
		// Large file reads still need at least one code marker.
		return countCodeMarkers(content) >= 1

	case "search", "grep":
		// Search/grep results are intentional API surface discovery.
		// Even a single file:line reference is signal — the agent
		// searched for it on purpose.
		if len(content) <= 4096 || countCodeMarkers(content) >= 1 {
			return true
		}

	case "bash":
		// CLI --help output carries interface contracts (flags,
		// subcommands, defaults) that code markers won't catch.
		if looksLikeHelp(content) {
			return true
		}
		// For large bash output, raise the code-marker bar: verbose
		// stack traces and build logs match file:line patterns many
		// times without carrying API surface information. A failed
		// `go test` stack trace can have dozens of file:line refs but
		// zero architectural value.
		if len(content) > 2048 {
			return countCodeMarkers(content) >= 5
		}
	}

	// Default content-based heuristic for tools without type-specific
	// overrides, or when tool metadata is missing.
	n := len(content)
	codeMarkers := countCodeMarkers(content)
	switch {
	case n <= 200:
		return codeMarkers >= 1
	case n <= 2048:
		return true
	default:
		return codeMarkers >= 3
	}
}

// isConfigOrDocExt reports whether path targets a configuration, documentation,
// or data-format file whose content carries architectural constraints despite
// having few or no code markers.
func isConfigOrDocExt(path string) bool {
	switch fileExt(path) {
	case ".json", ".yaml", ".yml", ".toml", ".md", ".cfg", ".ini", ".conf", ".env":
		return true
	}
	return false
}

// fileExt returns the extension from the last path segment, including the dot.
// "session/context_editor.go" → ".go", "docker-compose.yaml" → ".yaml",
// "Makefile" → "".
func fileExt(path string) string {
	base := path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		base = path[idx+1:]
	}
	if idx := strings.LastIndexByte(base, '.'); idx >= 0 {
		return base[idx:]
	}
	return ""
}

// looksLikeHelp reports whether content resembles CLI --help output.
// Detects "Usage:" headers and repeated "--flag" patterns characteristic
// of command-line help text.
func looksLikeHelp(content string) bool {
	scan := content
	if len(scan) > 1024 {
		scan = scan[:1024]
	}
	return strings.Contains(scan, "Usage:") ||
		strings.Contains(scan, "usage:") ||
		strings.Count(scan, "--") >= 2
}

// countCodeMarkers scans the first 4 KB of content for structural patterns
// that distinguish code/API surfaces from build noise.
func countCodeMarkers(content string) int {
	scan := content
	if len(scan) > 4096 {
		scan = scan[:4096]
	}
	markers := 0
	for _, line := range strings.Split(scan, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "func "),
			strings.HasPrefix(trimmed, "type "),
			strings.HasPrefix(trimmed, "import ("),
			strings.HasPrefix(trimmed, "package "),
			strings.HasPrefix(trimmed, "var ("),
			strings.HasPrefix(trimmed, "const ("),
			strings.HasPrefix(trimmed, "interface {"),
			strings.HasPrefix(trimmed, "struct {"):
			markers += 2 // strong signal — nearly certainly code
		case matchFileLine(trimmed):
			markers++ // file:line — grep/search evidence
		}
	}
	return markers
}

// matchFileLine reports whether s looks like a file:line reference —
// a pattern typical of grep, search, and diagnostic tool output.
func matchFileLine(s string) bool {
	if len(s) < 4 {
		return false
	}
	// Look for ":<digit>" — the universal file:line pattern.
	for i := 1; i < len(s)-1; i++ {
		if s[i] == ':' && s[i+1] >= '0' && s[i+1] <= '9' {
			return true
		}
	}
	return false
}

// buildToolCallMap walks messages once, extracting tool name and a short
// input summary from every assistant ToolCall, keyed by call ID.
func buildToolCallMap(msgs []model.Message) map[string]toolCallInfo {
	m := make(map[string]toolCallInfo)
	for _, msg := range msgs {
		if msg.Role != model.RoleAssistant {
			continue
		}
		for _, tc := range msg.ToolCalls {
			m[tc.ID] = toolCallInfo{
				name:  tc.Function.Name,
				input: summariseToolInput(tc.Function.Name, tc.Function.Arguments),
			}
		}
	}
	return m
}

// summariseToolInput extracts the most informative argument from a tool
// call's JSON arguments — typically the file path, command string, or
// search pattern.
func summariseToolInput(name string, args string) string {
	// For common tools, extract the primary target.
	switch name {
	case "read_file":
		if v := extractJSONString(args, "file_path"); v != "" {
			return shortPath(v)
		}
	case "bash":
		if v := extractJSONString(args, "command"); v != "" {
			return truncate(v, 80)
		}
	case "search", "grep":
		if v := extractJSONString(args, "pattern"); v != "" {
			return truncate(v, 60)
		}
		if v := extractJSONString(args, "query"); v != "" {
			return truncate(v, 60)
		}
	case "task":
		if v := extractJSONString(args, "kind"); v != "" {
			return "kind=" + v
		}
	}
	// Fallback: if args is short enough, use it directly.
	if len(args) <= 60 {
		return args
	}
	return ""
}

// buildSkeleton produces a structured placeholder for a cleared tool
// result. It includes the tool name, target, output size, and for code
// reads, the top-level type/function signatures found in the output.
//
// Examples:
//
//	[cleared: bash go build ./... (8.3KB)]
//	[cleared: read_file theme/theme.go (1.2KB) | Key: type Theme interface, func NewTheme]
func buildSkeleton(callID string, content string, toolMap map[string]toolCallInfo) string {
	info, ok := toolMap[callID]
	if !ok {
		// Tool call metadata missing (compacted/truncated history).
		return fmt.Sprintf("[cleared: unknown (%s)]", formatBytes(len(content)))
	}

	var b strings.Builder
	b.WriteString("[cleared: ")
	b.WriteString(info.name)

	if info.input != "" {
		b.WriteString(" ")
		b.WriteString(info.input)
	}

	b.WriteString(" (")
	b.WriteString(formatBytes(len(content)))
	b.WriteByte(')')

	// For file reads, extract key symbols so the LLM Compactor can
	// reconstruct the API surface in the summary.
	if info.name == "read_file" {
		symbols := extractKeySymbols(content)
		if symbols != "" {
			b.WriteString(" | Key: ")
			b.WriteString(symbols)
		}
	}

	b.WriteByte(']')
	return b.String()
}

// extractKeySymbols scans code content for top-level declarations —
// types, functions, interfaces — that define the API surface. These
// are the symbols the LLM Compactor needs to build an accurate summary.
//
// It handles Go generics (e.g. "type Repository[T any] interface {")
// by capturing the full declaration between keywords rather than
// splitting on whitespace, which loses type parameters.
func extractKeySymbols(content string) string {
	var symbols []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "type ") && strings.Contains(trimmed, "interface"):
			// "type Theme interface {" → "type Theme interface"
			// "type Repository[T any] interface {" → "type Repository[T any] interface"
			if idx := strings.Index(trimmed, " interface"); idx > 5 {
				symbols = append(symbols, strings.TrimSpace(trimmed[:idx+10]))
			}
		case strings.HasPrefix(trimmed, "type ") && strings.Count(trimmed, " ") == 1:
			// "type Theme" — type alias continuing on next line.
			continue
		case strings.HasPrefix(trimmed, "type "):
			// "type Theme struct {" → "type Theme"
			// "type Repository[T any] struct {" → "type Repository[T any]"
			// "type Color string" → "type Color"
			// End at the first of: space-brace, " =", or end of line.
			rest := trimmed[5:] // after "type "
			end := len(rest)
			for _, sep := range []string{" {", " =", "\t{"} {
				if i := strings.Index(rest, sep); i >= 0 && i < end {
					end = i
				}
			}
			symbols = append(symbols, "type "+strings.TrimSpace(rest[:end]))
		case strings.HasPrefix(trimmed, "func "):
			// "func NewTheme(base Base) *Theme {" → "func NewTheme(base Base) *Theme"
			if idx := strings.IndexByte(trimmed, '{'); idx > 0 {
				symbols = append(symbols, strings.TrimSpace(trimmed[:idx]))
			} else if idx := strings.IndexByte(trimmed, '('); idx > 5 {
				// "func (m *Model) Init() tea.Cmd" — method (no body on this line)
				symbols = append(symbols, trimmed)
			}
		}
		if len(symbols) >= 5 {
			break
		}
	}
	if len(symbols) == 0 {
		return ""
	}
	return strings.Join(symbols, "; ")
}

// extractJSONString does a minimal, allocation-cheap extraction of a
// single string field from flat JSON. It avoids encoding/json for hot
// paths. Returns "" if the field is absent or not a string.
func extractJSONString(raw, field string) string {
	key := `"` + field + `":`
	pos := strings.Index(raw, key)
	if pos < 0 {
		return ""
	}
	rest := raw[pos+len(key):]
	// Skip whitespace.
	rest = strings.TrimLeft(rest, " \t")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	// Scan to the closing unescaped quote.
	val, ok := scanJSONString(rest[1:])
	if !ok {
		return ""
	}
	return val
}

// scanJSONString reads a JSON string body until the closing unescaped
// quote. It handles \" but not other escape sequences (adequate for
// file paths, commands, and search patterns).
func scanJSONString(s string) (string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++ // skip the escaped char
			continue
		}
		if s[i] == '"' {
			return s[:i], true
		}
	}
	return "", false
}

// shortPath returns the last two segments of a path for display.
// "internal/session/context_editor.go" → "session/context_editor.go"
func shortPath(p string) string {
	// Find the second-to-last separator.
	last := strings.LastIndexByte(p, '/')
	if last < 0 {
		return p
	}
	second := strings.LastIndexByte(p[:last], '/')
	if second < 0 {
		return p
	}
	return p[second+1:]
}

// truncate cuts s to maxLen characters, appending "…" if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

// formatBytes returns a human-readable size string.
func formatBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
