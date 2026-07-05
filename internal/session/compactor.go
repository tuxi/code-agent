package session

import (
	"code-agent/internal/model"
	"context"
	"errors"
	"fmt"
	"strings"
)

// Compactor shrinks a session's history when it grows too large for the context
// window. Implementations decide HOW (drop vs. summarize); the loop decides WHEN
// (via Session.NeedCompaction). A Compactor must leave Messages in a state the
// provider accepts: system message first, and no tool-result message orphaned
// from the assistant tool_calls message it answers.
type Compactor interface {
	Compact(ctx context.Context, sess *Session) error
}

// summarizeSystemPrompt instructs the model to act as a context-compaction
// engine. The output is internal context, not a user-facing reply. The fixed
// section contract (P12.d, mirroring Gemini CLI's state_snapshot and Codex's
// handoff prompt) matters most for weak local models: a free-form "condense
// this" invites rambling, while named sections force coverage of exactly the
// state the agent needs to continue.
const summarizeSystemPrompt = `You are a context-compaction engine for a coding agent. Fold the conversation below into ONE updated cumulative summary so the agent can continue with no loss of essential context.

Output exactly these five sections, in this order, as markdown headings. Keep every section terse — bullet points over prose. Write "none" for an empty section.

## Goal
The user's current objective, requirements, and explicit constraints or preferences.

## Key knowledge & decisions
Decisions made and why; established facts about the system; APIs or conventions discovered.

## Files & state
Files inspected or modified, the substance of each change, and the current state of the work.

## Errors & fixes
Errors hit, their root causes, and how each was fixed — or that it is still open.

## Plan & next step
Open tasks and the immediate next step.

Do not invent facts. Do not address the user — this is internal context, not a reply. Drop verbose tool output, intermediate reasoning, and anything superseded.`

// summaryMarker frames the digest so the model reads it as established
// background rather than a fresh user request.
const summaryMarker = "[Summary of the earlier conversation, condensed to fit the context window. Treat this as established background, not a new request.]"

// LLMCompactor performs Claude Code–style summary compaction: older turns are
// summarized by the model into Session.Summary, and the history is rebuilt as
//
//	system → summary → recent messages
//
// The summary is cumulative — each compaction folds the newly dropped turns into
// the previous Summary — so context survives across many compactions instead of
// being truncated away.
type LLMCompactor struct {
	Provider    model.Provider
	ModelName   string
	Temperature float64

	// KeepRecentTokens is the approximate token budget for the verbatim tail —
	// how much recent conversation survives compaction uncompacted. Denominated
	// in tokens, not messages (P12.a): convergence requires the kept tail to be
	// bounded relative to the compaction threshold, and a message count bounds
	// nothing — 50 messages fit easily under a 128k window's threshold but can
	// exceed a 32k window's threshold outright, which made compaction loop
	// forever on local models. Sized as CompactThreshold × compact_keep_ratio.
	KeepRecentTokens int
}

func (c *LLMCompactor) Compact(ctx context.Context, sess *Session) error {
	if c.KeepRecentTokens <= 0 {
		return nil
	}
	if c.Provider == nil {
		return errors.New("LLMCompactor: nil model provider")
	}
	msgs := sess.Messages
	if len(msgs) == 0 {
		return nil
	}
	system := msgs[0]

	// Conversation = everything after the fixed header. The header is the system
	// message, plus the existing summary message when Summary is set (see the
	// Session.Summary invariant). Folding starts after it so the prior digest is
	// carried in sess.Summary, not re-summarized as a message.
	convStart := 1
	if sess.Summary != "" {
		convStart = 2
	}
	if convStart >= len(msgs) {
		return nil
	}
	conversation := msgs[convStart:]
	if len(conversation) < 2 {
		// The last message is always kept, so with one message there is nothing
		// to fold.
		return nil
	}

	// Recent tail: walk backward from the end accumulating approximate tokens
	// until the budget is spent. The last message is always kept — folding the
	// in-flight request mid-turn would leave the model nothing but the digest —
	// so even a tiny budget converges: everything else folds. (The loop's
	// ineffective-compaction guard backstops the pathological case where
	// system + summary alone bust the threshold.)
	recentStart := len(conversation) - 1
	budget := c.KeepRecentTokens - approxTokens(conversation[recentStart])
	for recentStart > 0 {
		cost := approxTokens(conversation[recentStart-1])
		if cost > budget {
			break
		}
		budget -= cost
		recentStart--
	}
	// Never begin the kept tail on a tool result whose parent assistant
	// tool_calls message would be left behind in the folded region (the provider
	// rejects orphaned results). Widening keeps MORE than the budget — validity
	// over thrift.
	for recentStart > 0 && conversation[recentStart].Role == model.RoleTool {
		recentStart--
	}
	toFold := conversation[:recentStart]
	recent := conversation[recentStart:]
	if len(toFold) == 0 {
		return nil
	}

	summary, err := c.summarize(ctx, sess.Summary, toFold)
	if err != nil {
		return err
	}
	sess.Summary = summary

	rebuilt := make([]model.Message, 0, 2+len(recent))
	rebuilt = append(rebuilt, system)
	rebuilt = append(rebuilt, summaryMessage(summary))
	rebuilt = append(rebuilt, recent...)
	sess.Messages = rebuilt
	return nil
}

// summarize asks the model to fold the dropped messages into the previous
// summary, producing the updated cumulative digest.
func (c *LLMCompactor) summarize(ctx context.Context, prev string, msgs []model.Message) (string, error) {
	var b strings.Builder
	if prev != "" {
		b.WriteString("Existing summary so far:\n")
		b.WriteString(prev)
		b.WriteString("\n\n")
	}
	b.WriteString("New conversation to fold into the summary:\n")
	b.WriteString(renderMessages(msgs))
	b.WriteString("\nProduce the updated cumulative summary.")

	resp, err := c.Provider.Complete(ctx, model.Request{
		Model:       c.ModelName,
		Temperature: c.Temperature,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: summarizeSystemPrompt},
			{Role: model.RoleUser, Content: b.String()},
		},
	})
	if err != nil {
		return "", fmt.Errorf("compaction summarize: %w", err)
	}
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return "", errors.New("compaction summarize: model returned an empty summary")
	}
	return summary, nil
}

// approxTokens estimates a message's token cost as chars/4, tool-call names and
// arguments included. Deliberately rough: the keep budget only needs the right
// order of magnitude — the true post-compaction size is still measured by the
// next model call's reported usage (see the loop's maybeCompact).
func approxTokens(m model.Message) int {
	n := len(m.Content)
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n / 4
}

// renderMessages flattens messages into plain text for the summarizer. Tool
// calls and their results are labeled so the model can see what was done.
func renderMessages(msgs []model.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case model.RoleUser:
			fmt.Fprintf(&b, "USER: %s\n", m.Content)
		case model.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&b, "ASSISTANT: %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT_TOOL_CALL: %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			}
		case model.RoleTool:
			fmt.Fprintf(&b, "TOOL_RESULT: %s\n", m.Content)
		case model.RoleSystem:
			fmt.Fprintf(&b, "SYSTEM: %s\n", m.Content)
		}
	}
	return b.String()
}

// summaryMessage renders the digest as the single message that stands in for all
// folded turns. It is a user-role message for broad provider compatibility (a
// second system message mid-conversation is not universally accepted).
func summaryMessage(summary string) model.Message {
	return model.Message{
		Role:    model.RoleUser,
		Content: summaryMarker + "\n\n" + summary,
	}
}

// SlidingWindowCompactor keeps the system message plus the most recent
// KeepRecentMessages messages and drops the rest. It is a cheap, lossy fallback
// (no summary) used to prove the compaction wiring; LLMCompactor is the real
// context-engineering path.
type SlidingWindowCompactor struct {
	KeepRecentMessages int
}

func (c *SlidingWindowCompactor) Compact(ctx context.Context, sess *Session) error {
	if c.KeepRecentMessages <= 0 {
		return nil
	}
	msgs := sess.Messages
	// +1 leaves room for the system message at index 0, which is always kept.
	if len(msgs) <= c.KeepRecentMessages+1 {
		return nil
	}

	// The naive window keeps the last KeepRecentMessages messages, but a
	// tool-result message is only valid when the assistant tool_calls message it
	// answers still precedes it. If the cut lands inside an
	// assistant(tool_calls) → tool(results) group it would orphan those results
	// and the provider rejects the next request. Walk the cut back to the group's
	// owning assistant so the window always begins at a self-contained boundary.
	start := len(msgs) - c.KeepRecentMessages
	for start > 1 && msgs[start].Role == model.RoleTool {
		start--
	}

	kept := make([]model.Message, 0, 1+len(msgs)-start)
	kept = append(kept, msgs[0]) // system
	kept = append(kept, msgs[start:]...)
	sess.Messages = kept

	return nil
}
