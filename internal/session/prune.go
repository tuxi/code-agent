package session

import (
	"fmt"
	"regexp"
	"unicode/utf8"

	"code-agent/internal/model"
)

// Tier-0 pruning (P12.c): deterministic context reclamation that runs before
// LLM summary compaction — no model call, so it is the first resort on slow
// local models where every summarize costs minutes. Mirrors OpenCode's
// tool-output pruning and the spirit of Claude Code's microcompaction: old tool
// results and intermediate reasoning are the least valuable bytes in history,
// and every mainstream compactor drops them first.

const (
	// pruneToolResultLimit: tool results longer than this (chars) outside the
	// protected tail get truncated.
	pruneToolResultLimit = 2000
	// pruneToolResultHead: how much of a truncated tool result's head survives —
	// enough to identify what the call returned, not to re-read it.
	pruneToolResultHead = 200
)

// thinkBlockRe matches the inline reasoning local thinking models emit in
// message content. Stripped from old assistant messages only: recent reasoning
// may still be referenced by the in-flight work.
var thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// PruneOldContext truncates oversized tool results and strips think-blocks from
// conversation messages OUTSIDE the protected recent tail (protectTokens is the
// same approximate-token budget the compactor keeps verbatim). It returns the
// number of characters removed.
//
// It only shrinks message contents in place — the system message, the summary
// message, and the protected tail are untouched, and no message is removed, so
// tool-call groups stay balanced and the history stays provider-valid.
func PruneOldContext(sess *Session, protectTokens int) int {
	msgs := sess.Messages
	convStart := 1
	if sess.Summary != "" {
		convStart = 2
	}
	if convStart >= len(msgs) {
		return 0
	}

	// Protected boundary: walk back from the end spending the token budget —
	// the same accumulation the compactor uses for its keep window. The last
	// message is always protected.
	protectStart := len(msgs) - 1
	budget := protectTokens - approxTokens(msgs[protectStart])
	for protectStart > convStart {
		cost := approxTokens(msgs[protectStart-1])
		if cost > budget {
			break
		}
		budget -= cost
		protectStart--
	}

	saved := 0
	for i := convStart; i < protectStart; i++ {
		m := &msgs[i]
		switch m.Role {
		case model.RoleTool:
			if len(m.Content) > pruneToolResultLimit {
				head := truncateAtRune(m.Content, pruneToolResultHead)
				dropped := len(m.Content) - len(head)
				m.Content = head + fmt.Sprintf("\n[pruned: %d chars of old tool output dropped to fit the context window]", dropped)
				saved += dropped
			}
		case model.RoleAssistant:
			if stripped := thinkBlockRe.ReplaceAllString(m.Content, ""); len(stripped) < len(m.Content) {
				saved += len(m.Content) - len(stripped)
				m.Content = stripped
			}
		}
	}
	return saved
}

// truncateAtRune cuts s to at most n bytes without splitting a UTF-8 rune.
func truncateAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
