package session

import (
	"code-agent/internal/model"
	"code-agent/internal/reference"
	"fmt"
	"time"
)

// Session is one continuous agent conversation. It owns the message history
// that persists across turns, plus a metadata bag for things the harness will
// attach later (workspace summary, git snapshot, token budget, compaction
// state).
//
// The agent loop reads from and appends to Messages; it does not own them. This
// is what lets a REPL keep context across turns while a one-shot command is
// just a session that runs a single turn.
type Session struct {
	// ID is the durable identity assigned at creation. It is the key under which
	// the session is persisted and the handle used to resume it.
	ID string
	// WorkspacePath is the absolute project root directory this session runs in,
	// resolved for the *current* process. It is conversation identity metadata, not
	// an event. On persistence it is NOT the durable identity — see Workspace: the
	// repository writes a portable WorkspaceRef and re-derives WorkspacePath on load
	// (re-anchor), so the value survives an iOS sandbox-path change. An empty path
	// means the server default workspace was used.
	WorkspacePath string

	// Workspace is the portable, persisted identity of WorkspacePath. The repository
	// fills it on create (relativize) and resolves it back into WorkspacePath on load.
	// See WorkspaceRef and docs/ios_workspace_path_spec.md.
	Workspace WorkspaceRef
	// Name is the human-readable display name for this conversation. It is set
	// after the first turn (truncated first user message) and may be replaced by
	// an LLM-generated title or a user-supplied name via PATCH. Empty means unset.
	Name string
	// Model is the wire model string this session last ran with — stored so a
	// listing can show it and a resume can report it.
	Model string

	Messages []model.Message
	Metadata map[string]any
	// GatewayAssetCache maps a content hash (and optional local asset identity)
	// to an ownership-bound Gateway asset reference. It persists no bytes, STS,
	// OSS URL, or local path, allowing a resumed session to avoid re-uploading an
	// unchanged screenshot under the same authenticated Gateway session.
	GatewayAssetCache map[string]model.GatewayAssetRef

	// ReferenceLedger holds session-scoped opaque MCP values. The model only
	// sees their $ref handles; raw values are restored solely to resolve a later
	// tool call in this same session.
	ReferenceLedger []reference.Entry

	// Summary is the running LLM-generated digest of conversation turns that have
	// been compacted out of Messages. It is empty until the first summarizing
	// compaction. INVARIANT: when Summary != "", Messages[1] is the rendered
	// summary message (Messages[0] is always the system prompt). Each subsequent
	// compaction folds newly dropped turns into this text, so the digest is
	// cumulative rather than a snapshot of the last drop.
	Summary string

	PromptTokens     int
	ContextWindow    int
	CompactThreshold int

	// Compactions is the observability log: one entry per compaction, in order.
	// Each is recorded pending and finalized when the next model call measures
	// the reclaimed size. See CompactionStats.
	Compactions []CompactionStats

	CreatedAt time.Time
	UpdatedAt time.Time
	// ArchivedAt hides the conversation from the default list without deleting
	// messages, events, unread sequence facts, or its managed worktree. Zero means
	// the conversation is active in the normal list.
	ArchivedAt time.Time
}

type HistoryRepair struct {
	FromIndex int
	Removed   int
	Reason    error
}

// IsEmpty reports whether the session has no conversation yet — only the initial
// system prompt, no turns. Such a session is a throwaway (e.g. a fresh REPL
// launch the user immediately /resume'd away from) and must not be persisted,
// or the session list fills with empty msgs=1 entries.
func (s *Session) IsEmpty() bool {
	return len(s.Messages) <= 1
}

// RemoveEmptyAssistantNoOps removes legacy assistant messages that contain
// neither text nor tool calls. They cannot be sent to OpenAI-compatible
// providers and are not part of a tool-call/result pairing, so removing them
// repairs a corrupted session without changing tool protocol semantics.
func (s *Session) RemoveEmptyAssistantNoOps() int {
	if len(s.Messages) == 0 {
		return 0
	}
	kept := s.Messages[:0]
	removed := 0
	for _, message := range s.Messages {
		if message.IsEmptyAssistantNoOp() {
			removed++
			continue
		}
		kept = append(kept, message)
	}
	s.Messages = kept
	return removed
}

func (s *Session) FirstInvalidToolCallIndex() (int, error) {

	for messageIndex, message := range s.Messages {
		if message.Role != model.RoleAssistant {
			continue
		}

		for toolCallIndex, call := range message.ToolCalls {
			if err := call.ValidateForHistory(); err != nil {
				return messageIndex, fmt.Errorf(
					"message %d tool call %d (%q) is invalid: %w",
					messageIndex,
					toolCallIndex,
					call.Function.Name,
					err,
				)
			}
		}
	}
	return -1, nil
}

func (s *Session) TruncateInvalidToolCallTail() *HistoryRepair {
	index, err := s.FirstInvalidToolCallIndex()
	if err == nil {
		return nil
	}

	removed := len(s.Messages) - index
	s.Messages = s.Messages[:index]
	return &HistoryRepair{
		FromIndex: index,
		Removed:   removed,
		Reason:    err,
	}
}
