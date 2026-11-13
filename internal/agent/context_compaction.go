package agent

import (
	"context"
	"errors"

	"code-agent/internal/session"
)

func contextPressure(s *session.Session) bool {
	budget := session.InputTokenBudget(s.ContextWindow)
	tokens := max(s.PromptTokens, session.EstimateContextTokens(s.Messages))
	if last := s.LastCompaction(); last != nil && !last.Finalized() {
		tokens = session.EstimateContextTokens(s.Messages)
	}
	return budget > 0 && tokens >= budget
}

// CompactContext bypasses thresholds and cache cooldown for explicit recovery
// and manual requests. It never executes tools or appends a user turn.
func (r *Runner) CompactContext(ctx context.Context, s *session.Session) (bool, error) {
	if r.Compactor == nil {
		return false, errors.New("context compaction is unavailable")
	}
	beforeLen, beforeSummary := len(s.Messages), s.Summary
	compactor := r.Compactor
	if llm, ok := compactor.(*session.LLMCompactor); ok {
		forced := *llm
		forced.KeepRecentTokens = max(1, min(forced.KeepRecentTokens, session.EstimateContextTokens(s.Messages)/4))
		compactor = &forced
	}
	if err := compactor.Compact(ctx, s); err != nil {
		return false, err
	}
	changed := len(s.Messages) != beforeLen || s.Summary != beforeSummary
	if changed {
		if last := s.LastCompaction(); last != nil && !last.Finalized() {
			// Recovery can fold again before any successful request measures
			// the first fold. Keep one pending before/after measurement.
			last.SummaryChars = len(s.Summary)
		} else {
			s.RecordCompaction(s.PromptTokens, len(s.Summary))
		}
	}
	return changed, nil
}
