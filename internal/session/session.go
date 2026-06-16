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
	Messages  []model.Message
	Metadata  map[string]any
	CreatedAt time.Time
	UpdatedAt time.Time
}
