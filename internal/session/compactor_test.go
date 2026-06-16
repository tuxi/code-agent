package session

import (
	"context"
	"strings"
	"testing"

	"code-agent/internal/model"
)

// fakeProvider returns a canned summary and records the request it received, so
// tests can assert what the summarizer was actually asked to fold.
type fakeProvider struct {
	reply   string
	calls   int
	lastReq model.Request
}

func (f *fakeProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	f.lastReq = req
	f.calls++
	return model.Response{Content: f.reply}, nil
}

// conversation is a realistic history: two tool-calling exchanges (one with a
// single call, one with parallel calls) each followed by a final answer.
func conversation() []model.Message {
	return []model.Message{
		{Role: model.RoleSystem, Content: "sys"},
		{Role: model.RoleUser, Content: "u1"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a"}}},
		{Role: model.RoleTool, ToolCallID: "a", Content: "ra"},
		{Role: model.RoleAssistant, Content: "f1"},
		{Role: model.RoleUser, Content: "u2"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "b"}, {ID: "c"}}},
		{Role: model.RoleTool, ToolCallID: "b", Content: "rb"},
		{Role: model.RoleTool, ToolCallID: "c", Content: "rc"},
		{Role: model.RoleAssistant, Content: "f2"},
	}
}

// assertValidSequence enforces the provider's invariant: every tool-result
// message must be answered by a tool_call from the most recent assistant turn,
// and the system message stays at index 0.
func assertValidSequence(t *testing.T, msgs []model.Message) {
	t.Helper()
	if len(msgs) == 0 || msgs[0].Role != model.RoleSystem {
		t.Fatalf("first message must be system, got %+v", msgs)
	}
	open := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case model.RoleTool:
			if !open[m.ToolCallID] {
				t.Fatalf("orphan tool message at %d: tool_call_id %q has no preceding assistant tool_calls",
					i, m.ToolCallID)
			}
		case model.RoleAssistant:
			open = map[string]bool{}
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		default:
			open = map[string]bool{}
		}
	}
}

// With KeepRecentMessages=3 the naive window would start at the tool result
// "rb", orphaning it from its parent assistant(tool_calls b,c). The safe cut
// must walk back to that assistant.
func TestSlidingWindowKeepsToolGroupIntact(t *testing.T) {
	sess := &Session{Messages: conversation()}

	c := &SlidingWindowCompactor{KeepRecentMessages: 3}
	if err := c.Compact(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	assertValidSequence(t, sess.Messages)
	if got := sess.Messages[1]; got.Role != model.RoleAssistant || len(got.ToolCalls) == 0 {
		t.Fatalf("expected the window to start at the owning assistant tool_calls message, got %+v", got)
	}
}

// When the cut already lands on a clean boundary, nothing is walked back.
func TestSlidingWindowCleanBoundary(t *testing.T) {
	sess := &Session{Messages: conversation()}

	c := &SlidingWindowCompactor{KeepRecentMessages: 4} // starts at assistant(b,c)
	if err := c.Compact(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	assertValidSequence(t, sess.Messages)
	// system + last 4 messages, kept verbatim.
	if len(sess.Messages) != 5 {
		t.Fatalf("expected 5 messages (system + 4), got %d", len(sess.Messages))
	}
}

func TestSlidingWindowNoOpWhenSmall(t *testing.T) {
	orig := conversation()
	sess := &Session{Messages: append([]model.Message(nil), orig...)}

	c := &SlidingWindowCompactor{KeepRecentMessages: 50}
	if err := c.Compact(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if len(sess.Messages) != len(orig) {
		t.Fatalf("history shorter than the window must be left untouched: got %d want %d",
			len(sess.Messages), len(orig))
	}
}

// The LLM compactor must rebuild history as system → summary → recent, store the
// digest on the session, and keep the recent window's tool groups intact.
func TestLLMCompactorBuildsSummaryLayout(t *testing.T) {
	fp := &fakeProvider{reply: "DIGEST-1"}
	sess := &Session{Messages: conversation()}

	c := &LLMCompactor{Provider: fp, ModelName: "m", KeepRecentMessages: 3}
	if err := c.Compact(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	if fp.calls != 1 {
		t.Fatalf("expected exactly one summarize call, got %d", fp.calls)
	}
	if sess.Summary != "DIGEST-1" {
		t.Fatalf("session summary = %q, want %q", sess.Summary, "DIGEST-1")
	}
	if sess.Messages[0].Role != model.RoleSystem {
		t.Fatalf("first message must be system, got %s", sess.Messages[0].Role)
	}
	sum := sess.Messages[1]
	if sum.Role != model.RoleUser || !strings.Contains(sum.Content, "DIGEST-1") {
		t.Fatalf("second message must be the summary, got %+v", sum)
	}
	assertValidSequence(t, sess.Messages)
}

// A second compaction must fold the existing Summary into the new digest (the
// prior summary is fed to the model) rather than discard it.
func TestLLMCompactorFoldsPriorSummary(t *testing.T) {
	// Post-first-compaction layout: system, summary, then a fresh exchange.
	msgs := []model.Message{
		{Role: model.RoleSystem, Content: "sys"},
		summaryMessage("DIGEST-1"),
		{Role: model.RoleUser, Content: "u2"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "b"}, {ID: "c"}}},
		{Role: model.RoleTool, ToolCallID: "b", Content: "rb"},
		{Role: model.RoleTool, ToolCallID: "c", Content: "rc"},
		{Role: model.RoleAssistant, Content: "f2"},
		{Role: model.RoleUser, Content: "u3"},
		{Role: model.RoleAssistant, Content: "f3"},
	}
	fp := &fakeProvider{reply: "DIGEST-2"}
	sess := &Session{Summary: "DIGEST-1", Messages: msgs}

	c := &LLMCompactor{Provider: fp, ModelName: "m", KeepRecentMessages: 2}
	if err := c.Compact(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	// The prior digest must have been handed to the summarizer.
	folded := fp.lastReq.Messages[len(fp.lastReq.Messages)-1].Content
	if !strings.Contains(folded, "DIGEST-1") {
		t.Fatalf("prior summary was not folded into the summarize request:\n%s", folded)
	}
	if sess.Summary != "DIGEST-2" {
		t.Fatalf("session summary = %q, want %q", sess.Summary, "DIGEST-2")
	}
	// Layout is still system → summary → recent, with exactly one summary message.
	if sess.Messages[1].Role != model.RoleUser || !strings.Contains(sess.Messages[1].Content, "DIGEST-2") {
		t.Fatalf("expected a single refreshed summary at index 1, got %+v", sess.Messages[1])
	}
	if got := sess.Messages[len(sess.Messages)-1].Content; got != "f3" {
		t.Fatalf("most recent message should be kept verbatim, got %q", got)
	}
	assertValidSequence(t, sess.Messages)
}

// Compaction must not call the model when there is nothing old to fold.
func TestLLMCompactorNoOpWhenSmall(t *testing.T) {
	fp := &fakeProvider{reply: "unused"}
	sess := &Session{Messages: conversation()}

	c := &LLMCompactor{Provider: fp, ModelName: "m", KeepRecentMessages: 50}
	if err := c.Compact(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if fp.calls != 0 {
		t.Fatalf("expected no summarize call when history fits, got %d", fp.calls)
	}
	if sess.Summary != "" {
		t.Fatalf("summary should stay empty, got %q", sess.Summary)
	}
}
