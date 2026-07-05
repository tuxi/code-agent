package session

// NeedCompaction reports whether the session's prompt has grown past the
// compaction threshold AND compacting again can plausibly help.
//
// The second condition is the convergence guard (P12.b): when the last
// measured compaction failed to land back under the threshold (AfterTokens ≥
// CompactThreshold — e.g. a threshold smaller than the system prompt plus
// summary alone), retrying immediately would burn another summarize call for
// the same outcome, every loop iteration, forever. Instead the session cools
// down until the prompt has grown meaningfully past that measurement. Same
// lesson Gemini CLI shipped as a failed-compression flag after their own
// compression loop (gemini-cli#16213, P0).
func (s *Session) NeedCompaction() bool {
	if s.CompactThreshold <= 0 {
		return false
	}
	if s.PromptTokens < s.CompactThreshold {
		return false
	}
	if last := s.lastCompaction(); last != nil && last.Finalized() && last.AfterTokens >= s.CompactThreshold {
		// Ineffective compaction on record: retry only after real growth, so a
		// pathological configuration degrades to a warning instead of a loop.
		return s.PromptTokens >= last.AfterTokens+s.CompactThreshold/10
	}
	return true
}

// lastCompaction returns the most recent compaction stat, nil when the session
// has never compacted. (Compactions is in-memory observability state; after a
// resume the guard re-arms on the first measured compaction.)
func (s *Session) lastCompaction() *CompactionStats {
	if len(s.Compactions) == 0 {
		return nil
	}
	return &s.Compactions[len(s.Compactions)-1]
}
