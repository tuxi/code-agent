package session

import (
	"code-agent/internal/model"
	"strings"
)

// ContextEditor prunes stale tool results from session history before
// compaction. It's a free (no LLM call) cleanup step that removes
// information the model no longer needs — old search results,
// intermediate file reads, spent command output.
//
// This mirrors Claude Code's context editing clear_tool_uses strategy
// applied before the LLM summarizer runs.
type ContextEditor struct {
	// KeepTurns is the number of most recent assistant turns to preserve
	// in full. A "turn" is one assistant message + its tool call/result
	// pairs. Default: 3.
	KeepTurns int
}

// Edit clears old tool results. It never removes messages — only their
// content is replaced with a placeholder marker. The message structure
// stays valid for resending to the provider. Errors are preserved.
//
// Edit is idempotent: calling it twice on the same session is a no-op
// after the first pass. Returns the number of messages edited.
func (e ContextEditor) Edit(sess *Session) int {
	keep := e.KeepTurns
	if keep <= 0 {
		keep = 3
	}
	msgs := sess.Messages
	cutoff := findCutoff(msgs, keep)

	edited := 0
	for i := 0; i < cutoff; i++ {
		m := &msgs[i]
		if m.Role == model.RoleTool && !isError(m.Content) {
			m.Content = "[tool result cleared]"
			edited++
		}
	}
	return edited
}

// findCutoff returns the index of the first message to KEEP. Messages before
// this index are candidates for pruning.
func findCutoff(msgs []model.Message, keepTurns int) int {
	turns := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant {
			turns++
			if turns >= keepTurns {
				return i
			}
		}
	}
	return 0
}

// isError reports whether a tool result should be preserved because it
// carries diagnostically useful information.
func isError(content string) bool {
	return strings.Contains(content, "error:") ||
		strings.Contains(content, "failed") ||
		strings.Contains(content, "exit status") ||
		strings.Contains(content, "failure=") // P4.1 observation marker
}
