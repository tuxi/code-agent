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
