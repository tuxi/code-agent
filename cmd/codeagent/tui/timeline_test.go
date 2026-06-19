package tui

import (
	"testing"
	"time"

	"code-agent/internal/agent"
)

// tool plays a full tool lifecycle (started → observed → finished) into a
// timeline, with the given observed failure label ("" / "none" = success).
func tool(t *Timeline, step int, name, failure, result string) {
	t.Apply(agent.Event{Kind: agent.EventToolStarted, Step: step, ToolName: name})
	t.Apply(agent.Event{Kind: agent.EventObserved, Step: step, Failure: failure})
	t.Apply(agent.Event{Kind: agent.EventToolFinished, Step: step, ToolName: name, Observation: result})
}

func TestToolMergesByStep(t *testing.T) {
	var tl Timeline
	tool(&tl, 1, "grep", "none", "3 matches")

	if len(tl.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(tl.Items))
	}
	it := tl.Items[0]
	if it.Kind != ItemTool || it.Name != "grep" || it.Status != StatusOK || it.Text != "3 matches" {
		t.Fatalf("merged tool item wrong: %+v", it)
	}
}

func TestObservedFailureSetsStatus(t *testing.T) {
	var tl Timeline
	tool(&tl, 1, "run_command", "test", "FAIL: 2 failing")

	it := tl.Items[0]
	if it.Status != StatusFail || it.Failure != "test" {
		t.Fatalf("want fail/test, got %v/%q", it.Status, it.Failure)
	}
}

// The reply is one timeline item, not the frame (§3.1 / §7).
func TestAssistantAndUserAreItems(t *testing.T) {
	var tl Timeline
	tl.Apply(agent.Event{Kind: agent.EventTurnStarted, Text: "fix the failing test"})
	tool(&tl, 1, "read_file", "none", "...")
	tl.Apply(agent.Event{Kind: agent.EventTurnFinished, Text: "Fixed it."})

	if len(tl.Items) != 3 {
		t.Fatalf("want user+tool+assistant = 3, got %d", len(tl.Items))
	}
	if tl.Items[0].Kind != ItemUser || tl.Items[0].Text != "fix the failing test" {
		t.Fatalf("first item should be the user prompt: %+v", tl.Items[0])
	}
	if tl.Items[2].Kind != ItemAssistant || tl.Items[2].Text != "Fixed it." {
		t.Fatalf("last item should be the assistant reply: %+v", tl.Items[2])
	}
}

// load_skill produces a Skill card, not a raw tool line (timeline.go suppresses
// the tool events for it).
func TestSkillCardSuppressesToolLine(t *testing.T) {
	var tl Timeline
	tl.Apply(agent.Event{Kind: agent.EventToolStarted, Step: 1, ToolName: "load_skill"})
	tl.Apply(agent.Event{Kind: agent.EventSkillLoaded, ToolName: "verify-change", Version: "1"})
	tl.Apply(agent.Event{Kind: agent.EventObserved, Step: 1, Failure: "none"})
	tl.Apply(agent.Event{Kind: agent.EventToolFinished, Step: 1, ToolName: "load_skill", Observation: "Loaded skill: verify-change"})

	if len(tl.Items) != 1 {
		t.Fatalf("want a single Skill item, got %d: %+v", len(tl.Items), tl.Items)
	}
	if tl.Items[0].Kind != ItemSkill || tl.Items[0].Name != "verify-change" || tl.Items[0].Version != "1" {
		t.Fatalf("skill card wrong: %+v", tl.Items[0])
	}
}

func TestCollapseGroupsAdjacentSuccesses(t *testing.T) {
	var tl Timeline
	for i := 1; i <= 4; i++ {
		tool(&tl, i, "read_file", "none", "ok")
	}
	got := Collapse(tl.Items)
	if len(got) != 1 {
		t.Fatalf("want 1 collapsed group, got %d", len(got))
	}
	if len(got[0].Children) != 4 {
		t.Fatalf("want 4 members in the group, got %d", len(got[0].Children))
	}
}

// A failure breaks a run and never collapses: ok ok | fail | ok ok → group, fail, group.
func TestCollapseStopsAtFailure(t *testing.T) {
	var tl Timeline
	tool(&tl, 1, "read_file", "none", "ok")
	tool(&tl, 2, "read_file", "none", "ok")
	tool(&tl, 3, "read_file", "compile", "boom")
	tool(&tl, 4, "read_file", "none", "ok")
	tool(&tl, 5, "read_file", "none", "ok")

	got := Collapse(tl.Items)
	if len(got) != 3 {
		t.Fatalf("want group+fail+group = 3 entries, got %d", len(got))
	}
	if len(got[0].Children) != 2 || len(got[2].Children) != 2 {
		t.Fatalf("groups should have 2 members each, got %d and %d", len(got[0].Children), len(got[2].Children))
	}
	if got[1].Status != StatusFail || len(got[1].Children) != 0 {
		t.Fatalf("middle entry should be a lone failure, got %+v", got[1])
	}
}

// Tool items carry a duration spanning started→finished, and every item gets a
// stable, unique ID (so cards can be addressed / matched to replay frames).
func TestItemIDsAndDuration(t *testing.T) {
	start := time.Now()
	var tl Timeline
	tl.Apply(agent.Event{Kind: agent.EventToolStarted, Step: 1, ToolName: "run_command", At: start})
	tl.Apply(agent.Event{Kind: agent.EventObserved, Step: 1, Failure: "none"})
	tl.Apply(agent.Event{Kind: agent.EventToolFinished, Step: 1, ToolName: "run_command", At: start.Add(3 * time.Second)})
	tl.Apply(agent.Event{Kind: agent.EventTurnFinished, Text: "done", At: start.Add(3 * time.Second)})

	if d := tl.Items[0].Duration(); d != 3*time.Second {
		t.Fatalf("tool duration = %v, want 3s", d)
	}
	seen := map[string]bool{}
	for _, it := range tl.Items {
		if it.ID == "" {
			t.Fatalf("item has no ID: %+v", it)
		}
		if seen[it.ID] {
			t.Fatalf("duplicate item ID %q", it.ID)
		}
		seen[it.ID] = true
	}
}

// Different tools don't collapse together.
func TestCollapseKeyedByTool(t *testing.T) {
	var tl Timeline
	tool(&tl, 1, "read_file", "none", "ok")
	tool(&tl, 2, "grep", "none", "ok")
	got := Collapse(tl.Items)
	if len(got) != 2 {
		t.Fatalf("read_file and grep must not merge; got %d entries", len(got))
	}
}
