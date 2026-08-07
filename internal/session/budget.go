package session

// NeedCompaction reports whether the session's prompt has grown past the
// compaction threshold. The cooldown and cache-affinity logic lives in the
// agent loop (Runner.maybeCompact) where runtime telemetry is available.
func (s *Session) NeedCompaction() bool {
	if s.CompactThreshold <= 0 {
		return false
	}
	return s.PromptTokens >= s.CompactThreshold
}

// LastCompaction returns the most recent compaction stat, nil when the session
// has never compacted.
func (s *Session) LastCompaction() *CompactionStats {
	if len(s.Compactions) == 0 {
		return nil
	}
	return &s.Compactions[len(s.Compactions)-1]
}
