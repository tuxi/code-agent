package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/tools"
)

func TestOfferNextStepToolBuildsStructuredCard(t *testing.T) {
	tool := NewOfferNextStepTool()
	input := json.RawMessage(`{
		"detail":"已修复 TypeError 并补了测试。",
		"question":"要把 matches 的比较逻辑也改严吗？",
		"options":[
			{"label":"改严","recommended":true},
			{"label":"保持现状","message":"先保持现状"}
		]
	}`)
	result, err := tool.Execute(context.Background(), tools.ExecutionContext{TurnID: "turn_42", CallID: "call_7"}, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.FollowUp == nil {
		t.Fatal("missing follow-up side channel")
	}
	offer := result.FollowUp
	if offer.TurnID != "turn_42" || offer.ID == "" || !offer.Dismissible {
		t.Fatalf("offer identity = %+v", offer)
	}
	if len(offer.Options) != 2 || offer.Options[0].ID != "opt_1" || offer.Options[0].Message != "改严" || !offer.Options[0].Recommended {
		t.Fatalf("options = %+v", offer.Options)
	}
	if offer.Options[1].Message != "先保持现状" || offer.Options[1].Recommended {
		t.Fatalf("second option = %+v", offer.Options[1])
	}
}

func TestOfferNextStepToolSupportsOpenEndedPrompt(t *testing.T) {
	result, err := NewOfferNextStepTool().Execute(context.Background(), tools.ExecutionContext{TurnID: "t", CallID: "c"}, json.RawMessage(`{
		"detail":"两份材料已完成。",
		"question":"接下来要我出哪种材料？"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.FollowUp == nil || len(result.FollowUp.Options) != 0 {
		t.Fatalf("open-ended offer = %+v", result.FollowUp)
	}
}

func TestOfferNextStepToolRejectsAmbiguousRecommendation(t *testing.T) {
	_, err := NewOfferNextStepTool().Execute(context.Background(), tools.ExecutionContext{TurnID: "t", CallID: "c"}, json.RawMessage(`{
		"detail":"done",
		"question":"next?",
		"options":[{"label":"A"},{"label":"B"}]
	}`))
	if err == nil {
		t.Fatal("expected missing recommendation to fail")
	}
}

func TestTerminalFinalizerEndsTurnWithoutSecondModelCall(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{{
		Text: "修复完成。",
		ToolCalls: []FauxToolCall{{Name: "offer_next_step", Args: map[string]any{
			"detail":   "TypeError 已修复，测试通过。",
			"question": "要顺手改严比较逻辑吗？",
			"options": []map[string]any{
				{"label": "改严", "recommended": true},
				{"label": "保持现状", "message": "先保持现状"},
			},
		}}},
	}})
	if err := h.Tools.Register(NewOfferNextStepTool()); err != nil {
		t.Fatal(err)
	}
	result, err := h.RunTurn("修复它")
	if err != nil {
		t.Fatal(err)
	}
	if h.Provider.CallCount != 1 {
		t.Fatalf("model calls = %d, want 1 for terminal finalizer", h.Provider.CallCount)
	}
	if result.Final != "修复完成。" || result.FollowUp == nil || result.FollowUp.Question != "要顺手改严比较逻辑吗？" {
		t.Fatalf("result = %+v", result)
	}
	finished := h.EventsOfType(EventTurnFinished)
	if len(finished) != 1 || finished[0].FollowUp == nil || finished[0].FollowUp.ID != result.FollowUp.ID {
		t.Fatalf("turn_finished = %+v", finished)
	}
	if len(h.EventsOfType(EventAssistantText)) != 0 {
		t.Fatal("terminal final answer must not also be emitted as assistant_text")
	}
	assistants := h.AssistantMessages()
	if len(assistants) != 1 || assistants[0].Content != "修复完成。" || len(assistants[0].ToolCalls) != 1 {
		t.Fatalf("assistant history = %+v", assistants)
	}
}

func TestInvalidFinalizerFallsBackToNormalLoop(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{
		{
			Text: "先给一个无效卡片。",
			ToolCalls: []FauxToolCall{{Name: "offer_next_step", Args: map[string]any{
				"detail":   "done",
				"question": "next?",
				"options":  []map[string]any{{"label": "A"}, {"label": "B"}},
			}}},
		},
		FauxText("最终答复"),
	})
	if err := h.Tools.Register(NewOfferNextStepTool()); err != nil {
		t.Fatal(err)
	}
	result, err := h.RunTurn("task")
	if err != nil {
		t.Fatal(err)
	}
	if h.Provider.CallCount != 2 || result.Final != "最终答复" || result.FollowUp != nil {
		t.Fatalf("calls=%d result=%+v", h.Provider.CallCount, result)
	}
}

func TestModelRequestRecordsOfferNextStepInBothToolViews(t *testing.T) {
	h := NewFauxHarness(t, []FauxStep{FauxText("done")})
	if err := h.Tools.Register(NewOfferNextStepTool()); err != nil {
		t.Fatal(err)
	}
	payload, err := h.Runner.RunTurn(context.Background(), h.Session, "task")
	if err != nil {
		t.Fatal(err)
	}
	_ = payload
	requests := h.EventsOfType(EventModelRequest)
	if len(requests) != 1 {
		t.Fatalf("model_request count = %d", len(requests))
	}
	foundName := false
	for _, name := range requests[0].ToolNames {
		if name == "offer_next_step" {
			foundName = true
			break
		}
	}
	if !foundName {
		t.Fatalf("tool_names = %v", requests[0].ToolNames)
	}
	if requests[0].ContextSnapshot == nil {
		t.Fatal("missing context snapshot")
	}
	foundSnapshot := false
	for _, tool := range requests[0].ContextSnapshot.Tools {
		if tool.Name == "offer_next_step" {
			foundSnapshot = true
			break
		}
	}
	if !foundSnapshot {
		t.Fatalf("snapshot tools = %+v", requests[0].ContextSnapshot.Tools)
	}
}
