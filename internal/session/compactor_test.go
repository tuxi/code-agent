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

// pad grows a label to exactly 40 chars so every message costs ~10 approximate
// tokens (chars/4) — token-budget tests then pick tails by predictable math.
func pad(s string) string {
	return s + strings.Repeat("x", 40-len(s))
}

// call builds a tool call whose arguments are padded to 40 chars, so an
// assistant tool_calls message costs ~10 approximate tokens per call.
func call(id string) model.ToolCall {
	return model.ToolCall{ID: id, Function: model.FunctionCall{Name: "t", Arguments: pad(id + "-args")}}
}

// conversation is a realistic history: two tool-calling exchanges (one with a
// single call, one with parallel calls) each followed by a final answer. Every
// message is padded to ~10 approximate tokens (the parallel tool_calls message
// is ~20) for the token-budget tests.
func conversation() []model.Message {
	return []model.Message{
		{Role: model.RoleSystem, Content: pad("sys")},
		{Role: model.RoleUser, Content: pad("u1")},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{call("a")}},
		{Role: model.RoleTool, ToolCallID: "a", Content: pad("ra")},
		{Role: model.RoleAssistant, Content: pad("f1")},
		{Role: model.RoleUser, Content: pad("u2")},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{call("b"), call("c")}},
		{Role: model.RoleTool, ToolCallID: "b", Content: pad("rb")},
		{Role: model.RoleTool, ToolCallID: "c", Content: pad("rc")},
		{Role: model.RoleAssistant, Content: pad("f2")},
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
// digest on the session, and keep the recent window's tool groups intact. The
// 25-token budget covers f2 and rc; rc is a tool result, so the cut must widen
// back over rb to the owning assistant(b,c) — proving both the token
// accumulation and the group-safety walk.
func TestLLMCompactorBuildsSummaryLayout(t *testing.T) {
	fp := &fakeProvider{reply: "DIGEST-1"}
	sess := &Session{Messages: conversation()}

	c := &LLMCompactor{Provider: fp, ModelName: "m", KeepRecentTokens: 25}
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
		{Role: model.RoleSystem, Content: pad("sys")},
		summaryMessage("DIGEST-1"),
		{Role: model.RoleUser, Content: pad("u2")},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{call("b"), call("c")}},
		{Role: model.RoleTool, ToolCallID: "b", Content: pad("rb")},
		{Role: model.RoleTool, ToolCallID: "c", Content: pad("rc")},
		{Role: model.RoleAssistant, Content: pad("f2")},
		{Role: model.RoleUser, Content: pad("u3")},
		{Role: model.RoleAssistant, Content: pad("f3")},
	}
	fp := &fakeProvider{reply: "DIGEST-2"}
	sess := &Session{Summary: "DIGEST-1", Messages: msgs}

	// 25 tokens keep u3+f3 (10 each) and stop before f2.
	c := &LLMCompactor{Provider: fp, ModelName: "m", KeepRecentTokens: 25}
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
	if got := sess.Messages[len(sess.Messages)-1].Content; got != pad("f3") {
		t.Fatalf("most recent message should be kept verbatim, got %q", got)
	}
	assertValidSequence(t, sess.Messages)
}

// Compaction must not call the model when there is nothing old to fold.
func TestLLMCompactorNoOpWhenSmall(t *testing.T) {
	fp := &fakeProvider{reply: "unused"}
	sess := &Session{Messages: conversation()}

	c := &LLMCompactor{Provider: fp, ModelName: "m", KeepRecentTokens: 10000}
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

// Convergence floor: a budget smaller than any single message still folds
// everything but the last message — the tail shrinks to its minimum instead of
// silently refusing to compact (the message-count design's failure mode).
func TestLLMCompactorTinyBudgetKeepsOnlyLastMessage(t *testing.T) {
	fp := &fakeProvider{reply: "DIGEST"}
	sess := &Session{Messages: conversation()}

	c := &LLMCompactor{Provider: fp, ModelName: "m", KeepRecentTokens: 1}
	if err := c.Compact(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if fp.calls != 1 {
		t.Fatalf("expected one summarize call, got %d", fp.calls)
	}
	// system → summary → the single most recent message.
	if len(sess.Messages) != 3 {
		t.Fatalf("expected 3 messages (system, summary, last), got %d", len(sess.Messages))
	}
	if got := sess.Messages[2].Content; got != pad("f2") {
		t.Fatalf("last message must survive verbatim, got %q", got)
	}
	assertValidSequence(t, sess.Messages)
}
