package session

import (
	"errors"
	"time"

	"code-agent/internal/model"
)

var (
	ErrForkAssetsUnsupported          = errors.New("session: fork does not support asset-bearing history")
	ErrForkManagedWorktreeUnsupported = errors.New("session: managed-worktree fork requires Phase C2")
)

// ForkHistoryInto copies the source's latest durable conversational checkpoint
// into a freshly-created child. The child's system prompt and workspace
// metadata remain authoritative, so a fork cannot smuggle stale workspace
// instructions or managed-worktree ownership into another session.
func ForkHistoryInto(child, source *Session, name string, now time.Time) error {
	if child == nil || source == nil {
		return errors.New("session: fork requires source and child")
	}
	if err := ValidateForkSource(source); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	system := append([]model.Message(nil), child.Messages...)
	child.Messages = system
	if len(source.Messages) > 1 {
		child.Messages = append(child.Messages, cloneMessages(source.Messages[1:])...)
	}
	child.Summary = source.Summary
	child.PromptTokens = source.PromptTokens
	child.Model = source.Model
	child.Compactions = append([]CompactionStats(nil), source.Compactions...)
	child.Name = name
	if child.Name == "" && source.Name != "" {
		child.Name = source.Name + " (fork)"
	}
	child.ArchivedAt = time.Time{}
	child.UpdatedAt = now
	return nil
}

// ValidateForkSource checks session-scoped state before a child is provisioned,
// so an unsupported fork cannot leave an empty reserved session behind.
func ValidateForkSource(source *Session) error {
	if source == nil {
		return errors.New("session: fork requires source")
	}
	if source.ExecutionPolicy() == ExecutionPolicyIsolatedWorktree {
		return ErrForkManagedWorktreeUnsupported
	}
	if len(source.GatewayAssetCache) > 0 || len(source.ReferenceLedger) > 0 {
		return ErrForkAssetsUnsupported
	}
	for _, message := range source.Messages {
		if len(message.Assets) > 0 || len(message.LocalAssets) > 0 {
			return ErrForkAssetsUnsupported
		}
	}
	return nil
}

func cloneMessages(messages []model.Message) []model.Message {
	out := make([]model.Message, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].ToolCalls = append([]model.ToolCall(nil), message.ToolCalls...)
		out[i].Assets = append([]model.GatewayAssetRef(nil), message.Assets...)
		out[i].LocalAssets = append([]model.LocalAssetRef(nil), message.LocalAssets...)
	}
	return out
}
