// Package truncate provides safe truncation for tool output that enters the
// model's context. "Safe" means two things a plain byte slice s[:max] gets
// wrong:
//
//   - Never cut inside a UTF-8 rune. Multi-byte text (中文 is 3 bytes per
//     character) sliced mid-rune yields invalid UTF-8, which some model APIs
//     reject outright and others render as mojibake.
//   - Prefer cutting at a line boundary. Dropping the partial last line keeps
//     every surviving line intact, so the model never sees half a URL, half a
//     number, or half a JSON field — fragments it is tempted to complete by
//     guessing. Only a single line larger than the whole budget is cut mid-line
//     (at a rune boundary).
//
// The marker states how many bytes were dropped, so the model knows the output
// is incomplete and how much is missing, rather than silently short.
package truncate

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// lineSearchWindow bounds how far a cut moves to land on a line boundary. Past
// this, giving up a full window of real content to align on a newline costs
// more than a mid-line (rune-safe) cut.
const lineSearchWindow = 1000

// Head keeps at most max bytes from the start of s and appends a marker with
// the omitted byte count. s is returned unchanged when it fits or max <= 0.
func Head(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if nl := strings.LastIndexByte(s[:cut], '\n'); nl > 0 && cut-nl <= lineSearchWindow {
		cut = nl
	}
	return s[:cut] + fmt.Sprintf("\n...<truncated: %d bytes omitted>", len(s)-cut)
}

// Tail keeps at most max bytes from the end of s and prepends a marker with
// the omitted byte count. s is returned unchanged when it fits or max <= 0.
func Tail(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	if nl := strings.IndexByte(s[start:], '\n'); nl >= 0 && nl < lineSearchWindow && start+nl+1 < len(s) {
		start += nl + 1
	}
	return fmt.Sprintf("...<truncated: %d bytes omitted>\n", start) + s[start:]
}
