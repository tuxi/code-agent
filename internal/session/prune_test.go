package session

import (
	"strings"
	"testing"

	"code-agent/internal/model"
)

// Tier-0 pruning must truncate oversized old tool results and strip think-blocks
// outside the protected tail, while leaving the system message, the summary, and
// the protected tail byte-for-byte intact.
func TestPruneOldContext(t *testing.T) {
	bigResult := strings.Repeat("A", 5000)
	withThink := "<think>" + strings.Repeat("r", 1000) + "</think>the answer"
	recentUser := strings.Repeat("u", 400)  // ~100 approx tokens
	recentFinal := strings.Repeat("f", 400) // ~100 approx tokens
	sess := &Session{
		Summary: "DIGEST",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			summaryMessage("DIGEST"),
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a"}}},
			{Role: model.RoleTool, ToolCallID: "a", Content: bigResult}, // old → truncated
			{Role: model.RoleAssistant, Content: withThink},             // old → think stripped
			{Role: model.RoleUser, Content: recentUser},                 // protected
			{Role: model.RoleAssistant, Content: recentFinal},           // protected
		},
	}

	// 250-token protection covers the two 100-token recent messages and stops at
	// the think-block assistant.
	saved := PruneOldContext(sess, 250)
	if saved == 0 {
		t.Fatal("expected pruning to reclaim characters")
	}

	pruned := sess.Messages[3].Content
	if len(pruned) >= len(bigResult) {
		t.Fatalf("old tool result was not truncated: %d chars", len(pruned))
	}
	if !strings.HasPrefix(pruned, strings.Repeat("A", pruneToolResultHead)) || !strings.Contains(pruned, "[pruned:") {
		t.Fatalf("truncated result must keep its head and carry the marker, got %q…", pruned[:80])
	}
	if got := sess.Messages[4].Content; strings.Contains(got, "<think>") || !strings.Contains(got, "the answer") {
		t.Fatalf("think-block not stripped (or answer lost): %q", got)
	}
	// Protected region and header untouched.
	if sess.Messages[5].Content != recentUser || sess.Messages[6].Content != recentFinal {
		t.Fatal("protected tail must be untouched")
	}
	if sess.Messages[0].Content != "sys" || !strings.Contains(sess.Messages[1].Content, "DIGEST") {
		t.Fatal("system/summary header must be untouched")
	}
	// No message removed: tool-call groups stay balanced by construction.
	if len(sess.Messages) != 7 {
		t.Fatalf("pruning must not remove messages, got %d", len(sess.Messages))
	}
}

// A short tool result outside the protection window is below the truncation
// limit and must be left alone — pruning targets the oversized, not the old.
func TestPruneOldContextLeavesSmallResults(t *testing.T) {
	small := strings.Repeat("B", pruneToolResultLimit-1)
	sess := &Session{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "sys"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a"}}},
			{Role: model.RoleTool, ToolCallID: "a", Content: small},
			{Role: model.RoleAssistant, Content: strings.Repeat("f", 400)},
		},
	}
	if saved := PruneOldContext(sess, 100); saved != 0 {
		t.Fatalf("nothing over the limit, but saved %d", saved)
	}
	if sess.Messages[2].Content != small {
		t.Fatal("small old tool result must be untouched")
	}
}
