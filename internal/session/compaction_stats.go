package session

import (
	"fmt"
	"time"
)

// CompactionStats records the effect of one compaction so long-running sessions
// are observable: how much context a compaction actually reclaimed, and how
// large the resulting summary is.
//
// AfterTokens — and everything derived from it — cannot be known when the
// compaction runs, because the post-compaction prompt size is only measured by
// the next model call's reported usage. So a stat is created PENDING by
// RecordCompaction and completed later by FinalizeCompaction. This is also why
// the loop must not fake a "0 tokens" state after compacting: the true size is a
// measurement, not an assumption.
type CompactionStats struct {
	BeforeTokens     int
	AfterTokens      int     // -1 until finalized by the next model call
	SavedTokens      int     // BeforeTokens - AfterTokens (can be negative if a summary grew the prompt)
	CompressionRatio float64 // SavedTokens / BeforeTokens
	SummaryChars     int
	CompactedAt      time.Time
}

// Finalized reports whether the post-compaction size has been measured yet.
func (s CompactionStats) Finalized() bool { return s.AfterTokens >= 0 }

func (s CompactionStats) String() string {
	return fmt.Sprintf("before=%d after=%d saved=%d ratio=%.1f%% summary=%dchars",
		s.BeforeTokens, s.AfterTokens, s.SavedTokens, s.CompressionRatio*100, s.SummaryChars)
}

// RecordCompaction appends a pending stat for a compaction that just happened.
// beforeTokens is the prompt size that triggered it; summaryChars is the size of
// the resulting digest. The post-compaction size is filled in by
// FinalizeCompaction once the next model call measures it.
func (s *Session) RecordCompaction(beforeTokens, summaryChars int) {
	s.Compactions = append(s.Compactions, CompactionStats{
		BeforeTokens: beforeTokens,
		AfterTokens:  -1,
		SummaryChars: summaryChars,
		CompactedAt:  time.Now(),
	})
}

// FinalizeCompaction fills in the measured post-compaction prompt size for the
// most recent compaction if it is still pending, and returns the completed stat
// (nil if there was nothing to finalize). measured is the PromptTokens reported
// by the first model call after the compaction.
func (s *Session) FinalizeCompaction(measured int) *CompactionStats {
	if len(s.Compactions) == 0 {
		return nil
	}
	last := &s.Compactions[len(s.Compactions)-1]
	if last.Finalized() {
		return nil
	}
	last.AfterTokens = measured
	last.SavedTokens = last.BeforeTokens - measured
	if last.BeforeTokens > 0 {
		last.CompressionRatio = float64(last.SavedTokens) / float64(last.BeforeTokens)
	}
	return last
}
