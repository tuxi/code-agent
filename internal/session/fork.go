package session

import (
	"errors"
	"fmt"
	"time"

	"code-agent/internal/model"
)

var (
	ErrForkAssetsUnsupported          = errors.New("session: fork does not support asset-bearing history")
	ErrForkManagedWorktreeUnsupported = errors.New("session: managed-worktree fork requires Phase C2")
	// ErrForkTurnUnavailable reports that a requested fork cut turn is not present
	// in the source's verbatim history: it was folded into the summary by
	// compaction, never existed, or is still in flight. The exact pre-cut history
	// cannot be reconstructed, so the fork fails closed rather than over-including
	// later turns.
	ErrForkTurnUnavailable = errors.New("session: fork turn is not available in verbatim history")
)

// ForkHistoryInto copies the source's latest durable conversational checkpoint
// into a freshly-created child. The child's system prompt and workspace
// metadata remain authoritative, so a fork cannot smuggle stale workspace
// instructions or managed-worktree ownership into another session.
func ForkHistoryInto(child, source *Session, name string, now time.Time) error {
	return ForkHistoryIntoThrough(child, source, "", name, now)
}

// ForkHistoryIntoThrough copies the source's durable history into a
// freshly-created child, optionally stopping after the turn identified by
// throughTurnID (the user message's OriginTurnID). An empty throughTurnID keeps
// the whole checkpoint, exactly like ForkHistoryInto. When set, the copied
// suffix ends at the last message of that turn — the next user message is the
// cut point — so the child never inherits later turns and the kept history is
// always a provider-valid tool-call/result boundary.
func ForkHistoryIntoThrough(child, source *Session, throughTurnID, name string, now time.Time) error {
	if child == nil || source == nil {
		return errors.New("session: fork requires source and child")
	}
	if err := ValidateForkHistory(source); err != nil {
		return err
	}
	cut, err := forkCutIndex(source, throughTurnID)
	if err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	system := append([]model.Message(nil), child.Messages...)
	child.Messages = system
	if cut > 1 {
		child.Messages = append(child.Messages, cloneMessages(source.Messages[1:cut])...)
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

// forkCutIndex resolves the exclusive upper bound of the copied history. An
// empty throughTurnID cuts at the end (whole checkpoint). Otherwise it returns
// the index of the first user message after the target turn, so the kept suffix
// ends exactly at that turn's final message; a missing target turn (compacted
// away or never existed) is ErrForkTurnUnavailable.
func forkCutIndex(source *Session, throughTurnID string) (int, error) {
	if throughTurnID == "" {
		return len(source.Messages), nil
	}
	start := -1
	for i, message := range source.Messages {
		if message.OriginTurnID == throughTurnID {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, fmt.Errorf("%w: %s", ErrForkTurnUnavailable, throughTurnID)
	}
	// A user message carries OriginTurnID; assistant/tool and the compaction
	// summary do not. The next user message is therefore the clean turn boundary.
	for i := start + 1; i < len(source.Messages); i++ {
		if source.Messages[i].OriginTurnID != "" {
			return i, nil
		}
	}
	return len(source.Messages), nil
}

// ValidateForkTurn reports whether throughTurnID can be resolved in the
// source's verbatim history. Callers use it before provisioning a child so an
// unsupported cut never leaves an empty reserved session behind.
func ValidateForkTurn(source *Session, throughTurnID string) error {
	if source == nil {
		return errors.New("session: fork requires source")
	}
	_, err := forkCutIndex(source, throughTurnID)
	return err
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
	return ValidateForkHistory(source)
}

// ValidateForkHistory rejects session-scoped state whose ownership cannot yet
// be transferred. Managed checkout metadata is intentionally not considered
// history: isolated-worktree forks provision fresh ownership for the child.
func ValidateForkHistory(source *Session) error {
	if source == nil {
		return errors.New("session: fork requires source")
	}
	if len(source.ReferenceLedger) > 0 {
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
