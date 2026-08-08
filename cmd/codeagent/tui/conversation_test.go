package tui

import (
	"strings"
	"testing"

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
	if len(out) != 1 || out[0].Tool == nil {
		t.Fatalf("observed should update the tool card, got %+v", out)
	}
	if out[0].Tool.Status != chat.ToolCompleted || out[0].Tool.CallID != "call1" {
		t.Fatalf("a clean observed should complete the card: %+v", out[0].Tool)
	}

	out = c.Apply(agent.Event{Kind: agent.EventToolFinished, ToolName: "read_file", CallID: "call1", Step: 1, Observation: "content"})
	if len(out) != 1 || out[0].Tool == nil {
		t.Fatalf("tool_finished should update the tool card, got %+v", out)
	}
	if out[0].Tool.Status != chat.ToolCompleted || out[0].Tool.Result != "content" {
		t.Fatalf("tool_finished should fill the result: %+v", out[0].Tool)
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
	if len(out) != 1 || out[0].Tool == nil || out[0].Tool.Status != chat.ToolFailed {
		t.Fatalf("a classified failure should mark the card failed, got %+v", out)
	}

	out = c.Apply(agent.Event{Kind: agent.EventToolFinished, ToolName: "run_command", CallID: "c1", Err: "boom"})
	if len(out) != 1 || out[0].Tool == nil || out[0].Tool.Status != chat.ToolFailed {
		t.Fatalf("a tool error should keep the card failed, got %+v", out)
	}
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
	c := &Conversation{}
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
		out := c.Apply(tc.ev)
		if len(out) != 1 || out[0].Kind != chat.KindSystem || !strings.Contains(out[0].Content, tc.sub) {
			t.Fatalf("%s should fold to a system message containing %q, got %+v", tc.ev.Kind, tc.sub, out)
		}
	}
}

func TestReflectionAndVerificationFoldToSystemMessages(t *testing.T) {
	c := &Conversation{}
	out := c.Apply(agent.Event{Kind: agent.EventReflected, Text: "self-check"})
	if len(out) != 1 || !strings.Contains(out[0].Content, "self-check") {
		t.Fatalf("reflection event = %+v", out)
	}
	out = c.Apply(agent.Event{Kind: agent.EventVerified})
	if len(out) != 1 || !strings.Contains(out[0].Content, "verification passed") {
		t.Fatalf("verification event = %+v", out)
	}
}
