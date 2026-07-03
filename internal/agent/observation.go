package agent

import "code-agent/internal/truncate"

// maxObservationBytes caps a single tool observation before it enters the
// conversation. Large enough that structured tool output (search results, file
// reads, diffs) usually arrives whole; the compaction threshold still bounds
// total context growth.
const maxObservationBytes = 30000

// TruncateObservation 给 observation 做统一截断。Rune- and line-safe (see
// internal/truncate): never yields invalid UTF-8 or a half-cut line, and the
// marker reports how many bytes were dropped.
func TruncateObservation(s string, max int) string {
	return truncate.Head(s, max)
}
