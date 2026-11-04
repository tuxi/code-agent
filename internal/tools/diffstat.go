package tools

import (
	"path/filepath"
	"strings"
)

// Write statuses carried by FileWrite.Status. They are the wire values of
// diffstat.files[].status (agent.DiffFile aliases FileWrite).
const (
	WriteAdded    = "added"
	WriteModified = "modified"
	WriteDeleted  = "deleted"
)

// FileWrite is one file's write contribution from one tool call.
//
// It exists so the runner never parses tool prose: a write tool reports what it
// actually put on disk, and the runner sums those reports into the turn-level
// diffstat. The type lives in tools (not agent) because tools may not import
// agent; the core aliases it as agent.DiffFile so both sides share one shape.
type FileWrite struct {
	// Path is workspace-relative, slash-separated, without a "./" prefix.
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Status  string `json:"status"`
}

// LineCount counts lines the way read_file and the apply_patch hunks do: split
// on "\n", drop the trailing empty segment, and treat "" as zero. Sharing this
// keeps create_file's "N lines" header, edit_file's replacement size and the
// diffstat from drifting apart by one line on files without a trailing newline.
func LineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// WorkspaceRel renders abs relative to rootAbs for the wire. Paths outside the
// root keep their absolute form rather than becoming "../..", because the
// consumer's only use for Path today is display and a later expand-list; a
// traversal-looking path would be worse than an absolute one it can recognize.
func WorkspaceRel(rootAbs, abs string) string {
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}
