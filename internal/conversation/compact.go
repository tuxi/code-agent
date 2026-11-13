package conversation

import (
	"context"
	"errors"
	"maps"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/session"
)

var ErrCompactionUnsupported = errors.New("conversation: context compaction is unavailable")

type contextCompactor interface {
	CompactContext(context.Context, *session.Session) (bool, error)
}

// CompactConversation uses the same exclusion as turn admission and archive.
// The original event transcript is untouched; only model context is replaced.
func (e *TurnExecutor) CompactConversation(ctx context.Context, id string) (*session.Session, bool, error) {
	release, err := e.beginExclusiveConversationOperation(ctx, id)
	if err != nil {
		return nil, false, err
	}
	defer release()
	original, err := e.repo.Load(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if !original.ArchivedAt.IsZero() {
		return nil, false, ErrConversationArchived
	}
	if original.TurnStatus() == session.TurnStatusWaitingBackground {
		return nil, false, &ConversationInUseError{SessionID: id, State: session.TurnStatusWaitingBackground}
	}
	// Providers and model resolution use the same session credential as turns.
	copy := *original
	copy.Metadata = maps.Clone(original.Metadata)
	if resolver, ok := e.rb.(ModelResolver); ok {
		mc, err := resolver.ResolveModel(copy.Model)
		if err != nil {
			return nil, false, err
		}
		if mc.ContextWindow > 0 {
			copy.ContextWindow = mc.ContextWindow
		}
		if mc.ContextWindow > 0 && mc.CompactRatio > 0 {
			copy.CompactThreshold = int(float64(mc.ContextWindow) * mc.CompactRatio)
		}
	}
	copy.Messages = append([]model.Message(nil), original.Messages...)
	copy.Compactions = append([]session.CompactionStats(nil), original.Compactions...)
	runner := e.rb.Build(RuntimeContext{Session: &copy, Model: copy.Model, ReasoningEffort: copy.ReasoningEffort, Credential: e.sessionCredential(id)})
	compactor, ok := runner.(contextCompactor)
	if !ok {
		return nil, false, ErrCompactionUnsupported
	}
	changed, err := compactor.CompactContext(ctx, &copy)
	if err != nil {
		return nil, false, err
	}
	if changed {
		copy.UpdatedAt = time.Now()
		if err := e.repo.Save(ctx, &copy); err != nil {
			return nil, false, err
		}
	}
	return &copy, changed, nil
}
