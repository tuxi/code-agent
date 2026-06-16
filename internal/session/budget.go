package session

func (s *Session) NeedCompaction() bool {
	if s.CompactThreshold <= 0 {
		return false
	}

	return s.PromptTokens >= s.CompactThreshold
}
