package agent

import (
	"strconv"
	"testing"

	"code-agent/internal/tools"
)

func TestTurnDiffStatSumsRepeatedWritesToOneFile(t *testing.T) {
	var s TurnDiffStat
	s.Add(tools.FileWrite{Path: "a.go", Added: 3, Removed: 1, Status: tools.WriteModified})
	s.Add(tools.FileWrite{Path: "a.go", Added: 2, Removed: 4, Status: tools.WriteModified})

	if s.FilesChanged != 1 {
		t.Fatalf("files_changed = %d, want 1 (same path merges)", s.FilesChanged)
	}
	if got := s.Files[0]; got.Added != 5 || got.Removed != 5 {
		t.Fatalf("file = %+v, want 5 added / 5 removed (per-call sum, not net)", got)
	}
	if s.AddedLines != 5 || s.RemovedLines != 5 {
		t.Fatalf("totals = %d/%d, want 5/5", s.AddedLines, s.RemovedLines)
	}
}

// A file created and then edited in the same turn is still "added": that is how
// the client explains why every line looks new.
func TestTurnDiffStatKeepsOriginStatus(t *testing.T) {
	var s TurnDiffStat
	s.Add(tools.FileWrite{Path: "new.go", Added: 10, Status: tools.WriteAdded})
	s.Add(tools.FileWrite{Path: "new.go", Added: 2, Removed: 1, Status: tools.WriteModified})

	if s.Files[0].Status != tools.WriteAdded {
		t.Fatalf("status = %q, want added", s.Files[0].Status)
	}
}

func TestTurnDiffStatDeletionWins(t *testing.T) {
	var s TurnDiffStat
	s.Add(tools.FileWrite{Path: "gone.go", Added: 1, Status: tools.WriteModified})
	s.Add(tools.FileWrite{Path: "gone.go", Removed: 7, Status: tools.WriteDeleted})

	if s.Files[0].Status != tools.WriteDeleted {
		t.Fatalf("status = %q, want deleted", s.Files[0].Status)
	}
	if s.RemovedLines != 7 {
		t.Fatalf("removed = %d, want 7", s.RemovedLines)
	}
}

func TestTurnDiffStatEmptyIsNilOnTheWirePath(t *testing.T) {
	var zero TurnDiffStat
	if !zero.Empty() {
		t.Fatal("zero stat should be empty")
	}
	if got := zero.Limited(200); got != nil {
		t.Fatalf("Limited on empty = %+v, want nil so the field is omitted", got)
	}
	var nilStat *TurnDiffStat
	if !nilStat.Empty() || nilStat.Limited(200) != nil {
		t.Fatal("nil stat must stay nil")
	}
}

// Truncation clips the list but never the summary: a turn that touched 300 files
// still reports files_changed=300 with 200 entries.
func TestTurnDiffStatLimitedKeepsFullTotals(t *testing.T) {
	var s TurnDiffStat
	for i := 0; i < 300; i++ {
		s.Add(tools.FileWrite{Path: "f" + strconv.Itoa(i), Added: i, Status: tools.WriteModified})
	}
	got := s.Limited(200)
	if len(got.Files) != 200 {
		t.Fatalf("files = %d, want 200", len(got.Files))
	}
	if got.FilesChanged != 300 {
		t.Fatalf("files_changed = %d, want the full 300", got.FilesChanged)
	}
	if got.AddedLines != s.AddedLines {
		t.Fatalf("added = %d, want %d", got.AddedLines, s.AddedLines)
	}
	if got.Files[0].Added < got.Files[199].Added {
		t.Fatal("truncated list should be ordered largest first")
	}
	// The receiver keeps everything: truncation is a wire concern only.
	if len(s.Files) != 300 {
		t.Fatalf("receiver mutated: %d files", len(s.Files))
	}
}
