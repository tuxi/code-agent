package tui

import (
	"strings"
	"testing"
	"time"

	"code-agent/cmd/codeagent/tui/components/chat"
	"code-agent/internal/agent"
)

func TestTurnStartedAppendsUserMessage(t *testing.T) {
	c := &Conversation{}
	out := c.Apply(agent.Event{Kind: agent.EventTurnStarted, Text: "fix the test"})
	if len(out) != 1 || out[0].Kind != chat.KindUser || out[0].Content != "fix the test" {
		t.Fatalf("turn_started should append a user message, got %+v", out)
	}
}

func TestTurnStartedWithoutTextHasNoPresence(t *testing.T) {
	c := &Conversation{}
	if out := c.Apply(agent.Event{Kind: agent.EventTurnStarted}); len(out) != 0 {
		t.Fatalf("an empty prompt should not append a user message, got %+v", out)
	}
}

func TestTokenDeltaStreamsAssistantMessage(t *testing.T) {
	c := &Conversation{}
	c.Apply(agent.Event{Kind: agent.EventTokenDelta, Text: "Hi"})
	out := c.Apply(agent.Event{Kind: agent.EventTokenDelta, Text: " there"})
	if len(out) != 1 {
		t.Fatalf("a delta should update the in-flight message, got %d messages", len(out))
	}
	if out[0].Kind != chat.KindAssistant || out[0].Content != "Hi there" || out[0].Finished {
		t.Fatalf("streaming message = %+v", out[0])
	}
}

func TestToolCardUpdatesInPlaceByCallID(t *testing.T) {
	c := &Conversation{}
	c.Apply(agent.Event{Kind: agent.EventToolStarted, ToolName: "read_file", ToolArgs: `{"path":"a.go"}`, CallID: "call1", Step: 1})

	out := c.Apply(agent.Event{Kind: agent.EventObserved, ToolName: "read_file", CallID: "call1", Step: 1, Observation: "1 file"})
	member := memberOf(t, out)
	if member.Status != chat.ToolCompleted || member.CallID != "call1" {
		t.Fatalf("a clean observed should complete the card: %+v", member)
	}

	out = c.Apply(agent.Event{Kind: agent.EventToolFinished, ToolName: "read_file", CallID: "call1", Step: 1, Observation: "content"})
	member = memberOf(t, out)
	if member.Status != chat.ToolCompleted || member.Result != "content" {
		t.Fatalf("tool_finished should fill the result: %+v", member)
	}

	toolCount := 0
	for _, m := range c.Messages() {
		if m.Kind == chat.KindTool {
			toolCount++
		}
	}
	if toolCount != 1 {
		t.Fatalf("the tool card must update in place, got %d tool cards", toolCount)
	}
}

func TestToolFailureSetsFailedStatus(t *testing.T) {
	c := &Conversation{}
	c.Apply(agent.Event{Kind: agent.EventToolStarted, ToolName: "run_command", ToolArgs: `{"command":"go test"}`, CallID: "c1"})

	out := c.Apply(agent.Event{Kind: agent.EventObserved, ToolName: "run_command", CallID: "c1", Failure: "compile"})
	if m := memberOf(t, out); m.Status != chat.ToolFailed {
		t.Fatalf("a classified failure should mark the card failed, got %+v", m)
	}

	out = c.Apply(agent.Event{Kind: agent.EventToolFinished, ToolName: "run_command", CallID: "c1", Err: "boom"})
	if m := memberOf(t, out); m.Status != chat.ToolFailed {
		t.Fatalf("a tool error should keep the card failed, got %+v", m)
	}
}

// memberOf extracts the sole tool member from a fold-group message, failing the
// test if the output is not a single tool group holding exactly one call.
func memberOf(t *testing.T, out []chat.Message) *chat.ToolCall {
	t.Helper()
	if len(out) != 1 || out[0].Kind != chat.KindTool || out[0].Fold == nil || len(out[0].Fold.ToolCalls) == 0 {
		t.Fatalf("expected a tool fold group, got %+v", out)
	}
	return &out[0].Fold.ToolCalls[0]
}

// foldText returns the full visible text of a message: its own content plus the
// member lines of a folded system group (grouped notices land in Fold.Lines).
func foldText(m chat.Message) string {
	if m.Fold == nil || len(m.Fold.Lines) == 0 {
		return m.Content
	}
	return strings.Join(append([]string{m.Content}, m.Fold.Lines...), " ")
}

func TestLoadSkillToolLineSuppressed(t *testing.T) {
	c := &Conversation{}
	for _, ev := range []agent.Event{
		{Kind: agent.EventToolStarted, ToolName: "load_skill", CallID: "s1"},
		{Kind: agent.EventObserved, ToolName: "load_skill", CallID: "s1"},
		{Kind: agent.EventToolFinished, ToolName: "load_skill", CallID: "s1"},
	} {
		if out := c.Apply(ev); len(out) != 0 {
			t.Fatalf("load_skill %s should be suppressed, got %+v", ev.Kind, out)
		}
	}
}

func TestSkillLoadedAppendsToolCard(t *testing.T) {
	c := &Conversation{}
	out := c.Apply(agent.Event{Kind: agent.EventSkillLoaded, ToolName: "verify-change", Version: "2", CallID: "skill1"})
	if len(out) != 1 || out[0].Kind != chat.KindTool || out[0].Tool == nil {
		t.Fatalf("skill load should append a tool card, got %+v", out)
	}
	if out[0].Tool.Name != "verify-change" || out[0].Tool.Status != chat.ToolCompleted {
		t.Fatalf("skill card = %+v", out[0].Tool)
	}
}

func TestCompactedAppendsCompactMessage(t *testing.T) {
	c := &Conversation{}
	out := c.Apply(agent.Event{Kind: agent.EventCompacted, BeforeTokens: 5000, AfterTokens: 3000, SavedTokens: 2000})
	if len(out) != 1 || out[0].Kind != chat.KindCompact {
		t.Fatalf("compaction should append a compact message, got %+v", out)
	}
}

func TestPlanEventsFoldToSystemMessages(t *testing.T) {
	c := &Conversation{}
	cases := []struct {
		ev  agent.Event
		sub string
	}{
		{agent.Event{Kind: agent.EventPlanStateChanged, PlanState: agent.PlanStatusPlanning}, "planning"},
		{agent.Event{Kind: agent.EventPlanProposed, Text: "# Refactor loop\nbody"}, "Refactor loop"},
		{agent.Event{Kind: agent.EventPlanApproved}, "approved"},
		{agent.Event{Kind: agent.EventPlanRejected}, "rejected"},
	}
	for _, tc := range cases {
		out := c.Apply(tc.ev)
		if len(out) != 1 || out[0].Kind != chat.KindSystem || !strings.Contains(out[0].Content, tc.sub) {
			t.Fatalf("%s should fold to a system message containing %q, got %+v", tc.ev.Kind, tc.sub, out)
		}
	}
}

func TestLifecycleAndJobEventsFoldToSystemMessages(t *testing.T) {
	cases := []struct {
		ev  agent.Event
		sub string
	}{
		{agent.Event{Kind: agent.EventTurnPaused}, "paused"},
		{agent.Event{Kind: agent.EventTurnResumed}, "resumed"},
		{agent.Event{Kind: agent.EventTurnCancelled}, "cancelled"},
		{agent.Event{Kind: agent.EventTurnFailed, Err: "boom"}, "boom"},
		{agent.Event{Kind: agent.EventTaskStarted, Text: "explore the codebase"}, "explore"},
		{agent.Event{Kind: agent.EventTaskFinished}, "task finished"},
		{agent.Event{Kind: agent.EventAskUserPosted, Text: "which approach?"}, "which approach?"},
		{agent.Event{Kind: agent.EventAskUserResolved, Text: "Option A"}, "Option A"},
		{agent.Event{Kind: agent.EventAskUserTimeout, Text: "no user"}, "no user"},
		{agent.Event{Kind: agent.EventJobStarted, Text: "go test"}, "go test"},
		{agent.Event{Kind: agent.EventJobOutput, Chunk: "line"}, "line"},
		{agent.Event{Kind: agent.EventJobFinished, Text: "exited 0"}, "exited 0"},
		{agent.Event{Kind: agent.EventAutoApproved, ToolName: "edit_file"}, "edit_file"},
		{agent.Event{Kind: agent.EventTodoUpdated}, "todo"},
	}
	for _, tc := range cases {
		// A fresh conversation per case: folded notices stay independent so the
		// assertion checks this event alone, not what it merged into.
		c := &Conversation{}
		out := c.Apply(tc.ev)
		if len(out) != 1 || out[0].Kind != chat.KindSystem || !strings.Contains(foldText(out[0]), tc.sub) {
			t.Fatalf("%s should fold to a system message containing %q, got %+v", tc.ev.Kind, tc.sub, out)
		}
	}
}

func TestReflectionAndVerificationFoldToSystemMessages(t *testing.T) {
	c := &Conversation{}
	out := c.Apply(agent.Event{Kind: agent.EventReflected, Text: "self-check"})
	if len(out) != 1 || !strings.Contains(foldText(out[0]), "self-check") {
		t.Fatalf("reflection event = %+v", out)
	}
	// Verification directly after a reflection merges into the same group.
	out = c.Apply(agent.Event{Kind: agent.EventVerified})
	if len(out) != 1 || !strings.Contains(foldText(out[0]), "verification passed") {
		t.Fatalf("verification event = %+v", out)
	}
}

func TestThinkingStreamsIntoFoldedBlock(t *testing.T) {
	c := &Conversation{}
	out := c.Apply(agent.Event{Kind: agent.EventReasoningDelta, Text: "step one"})
	if len(out) != 1 || out[0].Kind != chat.KindThinking || out[0].Fold == nil {
		t.Fatalf("a reasoning delta should open a folded thinking block, got %+v", out)
	}
	if !out[0].Fold.Running || out[0].Fold.Title != "Thought" {
		t.Fatalf("streaming thought should be a Running 'Thought' fold: %+v", out[0].Fold)
	}

	out = c.Apply(agent.Event{Kind: agent.EventReasoningDelta, Text: " more"})
	if len(out) != 1 || out[0].Content != "step one more" {
		t.Fatalf("deltas should accumulate in place, got %+v", out)
	}

	out = c.Apply(agent.Event{Kind: agent.EventThinking, Text: "authoritative"})
	if len(out) != 1 || out[0].Content != "authoritative" || !out[0].Finished {
		t.Fatalf("thinking should replace the streamed preview and finish, got %+v", out)
	}
	if out[0].Fold.Running {
		t.Fatalf("a finished thought is not Running")
	}
}

func TestToolGroupMergesConsecutiveSameName(t *testing.T) {
	c := &Conversation{}
	for _, id := range []string{"c1", "c2", "c3"} {
		out := c.Apply(agent.Event{Kind: agent.EventToolStarted, ToolName: "read_file", CallID: id, Step: 1})
		if len(out) != 1 || out[0].Fold == nil || out[0].Fold.Count != len(out[0].Fold.ToolCalls) {
			t.Fatalf("group update should carry the group, got %+v", out)
		}
	}
	groups := 0
	for _, m := range c.Messages() {
		if m.Kind == chat.KindTool && m.Fold != nil {
			groups++
			if m.Fold.Count != 3 || len(m.Fold.ToolCalls) != 3 {
				t.Fatalf("group should hold 3 members, got %+v", m.Fold)
			}
		}
	}
	if groups != 1 {
		t.Fatalf("three consecutive read_file calls should fold into one group, got %d", groups)
	}

	// Completing the last member stops the running flag (the list then folds
	// the group back to its summary line).
	c.Apply(agent.Event{Kind: agent.EventToolFinished, ToolName: "read_file", CallID: "c1", Observation: "a"})
	c.Apply(agent.Event{Kind: agent.EventToolFinished, ToolName: "read_file", CallID: "c2", Observation: "b"})
	out := c.Apply(agent.Event{Kind: agent.EventToolFinished, ToolName: "read_file", CallID: "c3", Observation: "c"})
	if len(out) != 1 || out[0].Fold == nil || out[0].Fold.Running {
		t.Fatalf("group should stop running when all members finish: %+v", out)
	}
}

func TestToolGroupSplitsOnInterleavedTool(t *testing.T) {
	c := &Conversation{}
	c.Apply(agent.Event{Kind: agent.EventToolStarted, ToolName: "read_file", CallID: "c1", Step: 1})
	c.Apply(agent.Event{Kind: agent.EventToolStarted, ToolName: "run_command", CallID: "c2", Step: 2})
	c.Apply(agent.Event{Kind: agent.EventToolStarted, ToolName: "read_file", CallID: "c3", Step: 3})
	groups := 0
	for _, m := range c.Messages() {
		if m.Kind == chat.KindTool && m.Fold != nil {
			groups++
		}
	}
	if groups != 3 {
		t.Fatalf("interleaved tools must not merge into one group, got %d groups", groups)
	}
}

func TestSystemNoticesGroupConsecutively(t *testing.T) {
	c := &Conversation{}
	c.Apply(agent.Event{Kind: agent.EventReflected, Text: "first"})
	out := c.Apply(agent.Event{Kind: agent.EventReflected, Text: "second"})
	if len(out) != 1 || out[0].Fold == nil || out[0].Fold.Count != 2 || len(out[0].Fold.Lines) != 2 {
		t.Fatalf("consecutive notices should merge into one group, got %+v", out)
	}

	// A high-signal event breaks the group: the next notice starts fresh.
	c.Apply(agent.Event{Kind: agent.EventAskUserPosted, Text: "question?"})
	out = c.Apply(agent.Event{Kind: agent.EventReflected, Text: "third"})
	if len(out) != 1 || out[0].Fold == nil || out[0].Fold.Count != 1 {
		t.Fatalf("a new group starts after a flat event: %+v", out)
	}
}

func TestTurnFooterAccumulatesTokensAndElapsed(t *testing.T) {
	c := &Conversation{}
	c.Apply(agent.Event{Kind: agent.EventTurnStarted, Text: "do it"})
	c.Apply(agent.Event{Kind: agent.EventModelFinished, TotalTokens: 1200, Elapsed: 3 * time.Second})
	c.Apply(agent.Event{Kind: agent.EventModelFinished, TotalTokens: 800, Elapsed: 2 * time.Second})
	out := c.Apply(agent.Event{Kind: agent.EventTurnFinished, Text: "done"})

	var footer *chat.Message
	for i := range out {
		if out[i].Kind == chat.KindSystem {
			footer = &out[i]
		}
	}
	if footer == nil || !strings.Contains(footer.Content, "tokens") {
		t.Fatalf("turn_finished should emit a cost footer, got %+v", out)
	}
	if !strings.Contains(footer.Content, "2.0k") || !strings.Contains(footer.Content, "5.0s") {
		t.Fatalf("footer should sum tokens and elapsed: %q", footer.Content)
	}
}

func TestResumeReplayIsDeterministic(t *testing.T) {
	events := []agent.Event{
		{Kind: agent.EventTurnStarted, Text: "fix it"},
		{Kind: agent.EventReasoningDelta, Text: "hmm"},
		{Kind: agent.EventThinking, Text: "snapshot"},
		{Kind: agent.EventToolStarted, ToolName: "grep", CallID: "g1", Step: 1},
		{Kind: agent.EventToolFinished, ToolName: "grep", CallID: "g1", Observation: "hit"},
		{Kind: agent.EventTokenDelta, Text: "Found "},
		{Kind: agent.EventTurnFinished, Text: "Found it."},
	}
	replay := func() []chat.Message {
		c := &Conversation{}
		for _, ev := range events {
			c.Apply(ev)
		}
		return c.Messages()
	}
	a, b := replay(), replay()
	if len(a) != len(b) {
		t.Fatalf("replays differ in length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Kind != b[i].Kind || a[i].Finished != b[i].Finished {
			t.Fatalf("replay %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// A live multi-step turn (text → tools → text → final) must yield one
// assistant block per model invocation, finalized at the next
// EventModelStarted, with the tool group between them — not one merged block
// with the final answer replacing everything.
func TestModelStartedSplitsPerStepAssistantBlocks(t *testing.T) {
	c := &Conversation{}
	c.Apply(agent.Event{Kind: agent.EventTurnStarted, Text: "do it"})
	// Step 1 streams text, then calls a tool.
	c.Apply(agent.Event{Kind: agent.EventTokenDelta, Text: "step one "})
	c.Apply(agent.Event{Kind: agent.EventToolStarted, ToolName: "grep", CallID: "g1", Step: 1})
	c.Apply(agent.Event{Kind: agent.EventToolFinished, ToolName: "grep", CallID: "g1", Observation: "hit"})
	// Step 2 begins: step 1's block must be finalized (finished, text kept).
	out := c.Apply(agent.Event{Kind: agent.EventModelStarted})
	if len(out) != 1 || out[0].Kind != chat.KindAssistant || !out[0].Finished || out[0].Content != "step one " {
		t.Fatalf("model_started should finalize step 1's block, got %+v", out)
	}
	// Step 2 streams its own text, then the turn ends.
	c.Apply(agent.Event{Kind: agent.EventTokenDelta, Text: "step two"})
	c.Apply(agent.Event{Kind: agent.EventTurnFinished, Text: "final answer"})

	var kinds []chat.Kind
	for _, m := range c.Messages() {
		kinds = append(kinds, m.Kind)
	}
	want := []chat.Kind{chat.KindUser, chat.KindAssistant, chat.KindTool, chat.KindAssistant}
	if len(kinds) != len(want) {
		t.Fatalf("transcript kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("transcript kinds = %v, want %v", kinds, want)
		}
	}
	msgs := c.Messages()
	if msgs[1].Content != "step one " || !msgs[1].Finished {
		t.Fatalf("step 1 block = %+v", msgs[1])
	}
	if msgs[3].Content != "final answer" || !msgs[3].Finished {
		t.Fatalf("final block = %+v", msgs[3])
	}
}

// A resumed multi-turn session must replay into a strictly chronological
// transcript: each turn is user → thinking → … → final answer, in order.
func TestResumeReplayPreservesChronologicalOrder(t *testing.T) {
	events := []agent.Event{
		{Kind: agent.EventTurnStarted, Text: "first turn"},
		{Kind: agent.EventThinking, Text: "t1"},
		{Kind: agent.EventTurnFinished, Text: "answer 1"},
		{Kind: agent.EventTurnStarted, Text: "second turn"},
		{Kind: agent.EventThinking, Text: "t2"},
		{Kind: agent.EventToolStarted, ToolName: "grep", CallID: "g1", Step: 1},
		{Kind: agent.EventToolFinished, ToolName: "grep", CallID: "g1", Observation: "hit"},
		{Kind: agent.EventTurnFinished, Text: "answer 2"},
	}
	c := &Conversation{}
	for _, ev := range events {
		c.Apply(ev)
	}
	var kinds []chat.Kind
	for _, m := range c.Messages() {
		kinds = append(kinds, m.Kind)
	}
	want := []chat.Kind{
		chat.KindUser, chat.KindThinking, chat.KindAssistant,
		chat.KindUser, chat.KindThinking, chat.KindTool, chat.KindAssistant,
	}
	if len(kinds) != len(want) {
		t.Fatalf("replayed kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("replayed kinds = %v, want %v", kinds, want)
		}
	}
	msgs := c.Messages()
	if msgs[2].Content != "answer 1" || msgs[6].Content != "answer 2" {
		t.Fatalf("final answers misplaced: %+v", msgs)
	}
}
