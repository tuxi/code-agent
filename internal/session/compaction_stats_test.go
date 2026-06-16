package session

import (
	"math"
	"testing"
)

func TestCompactionStatsLifecycle(t *testing.T) {
	s := &Session{}

	s.RecordCompaction(90000, 1800)
	if len(s.Compactions) != 1 {
		t.Fatalf("expected 1 recorded compaction, got %d", len(s.Compactions))
	}
	if s.Compactions[0].Finalized() {
		t.Fatal("a freshly recorded compaction must be pending, not finalized")
	}

	stat := s.FinalizeCompaction(27000)
	if stat == nil {
		t.Fatal("FinalizeCompaction returned nil for a pending stat")
	}
	if stat.AfterTokens != 27000 || stat.SavedTokens != 63000 {
		t.Fatalf("after=%d saved=%d, want 27000/63000", stat.AfterTokens, stat.SavedTokens)
	}
	if math.Abs(stat.CompressionRatio-0.7) > 1e-9 {
		t.Fatalf("ratio=%v, want 0.7", stat.CompressionRatio)
	}
	if !s.Compactions[0].Finalized() {
		t.Fatal("stat should be finalized after FinalizeCompaction")
	}

	// A second finalize must not touch the already-completed stat.
	if again := s.FinalizeCompaction(50000); again != nil {
		t.Fatalf("double finalize should be a no-op, got %+v", again)
	}
	if s.Compactions[0].AfterTokens != 27000 {
		t.Fatalf("after-tokens overwritten by double finalize: %d", s.Compactions[0].AfterTokens)
	}
}

func TestFinalizeCompactionNoPending(t *testing.T) {
	s := &Session{}
	if s.FinalizeCompaction(1000) != nil {
		t.Fatal("finalize with no recorded compaction must return nil")
	}
}
