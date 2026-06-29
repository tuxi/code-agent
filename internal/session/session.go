package session

import (
	"code-agent/internal/model"
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
	// WorkspacePath is the absolute project root directory this session runs in.
	// It is conversation identity metadata, not an event — set at creation time
	// and never derived from the event stream. An empty path means the server
	// default workspace was used.
	WorkspacePath string
	// Name is the human-readable display name for this conversation. It is set
	// after the first turn (truncated first user message) and may be replaced by
	// an LLM-generated title or a user-supplied name via PATCH. Empty means unset.
	Name string
	// Model is the wire model string this session last ran with — stored so a
	// listing can show it and a resume can report it.
	Model string

	Messages []model.Message
	Metadata map[string]any

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
}

// IsEmpty reports whether the session has no conversation yet — only the initial
// system prompt, no turns. Such a session is a throwaway (e.g. a fresh REPL
// launch the user immediately /resume'd away from) and must not be persisted,
// or the session list fills with empty msgs=1 entries.
func (s *Session) IsEmpty() bool {
	return len(s.Messages) <= 1
}
