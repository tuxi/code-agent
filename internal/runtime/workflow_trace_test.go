package runtime

import (
	"strings"
	"testing"

	"code-agent/internal/model"
)

func TestExtractToolTracePairsCallsWithResults(t *testing.T) {
	msgs := []model.Message{
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
			{ID: "call_1", Type: "function", Function: model.FunctionCall{Name: "mobile_launch_app", Arguments: `{"bundle_id":"com.atebits.Tweetie2"}`}},
		}},
		{Role: model.RoleTool, ToolCallID: "call_1", Content: "launched"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
			{ID: "call_2", Type: "function", Function: model.FunctionCall{Name: "web_search", Arguments: `{"query":"AI 热点"}`}},
		}},
		{Role: model.RoleTool, ToolCallID: "call_2", Content: "3 results"},
	}
	steps := extractToolTrace(msgs, 10)
	if len(steps) != 2 {
		t.Fatalf("steps=%d, want 2", len(steps))
	}
	if steps[0].Tool != "mobile_launch_app" || steps[0].Result != "launched" {
		t.Fatalf("step0=%+v", steps[0])
	}
	if steps[1].Tool != "web_search" || steps[1].Result != "3 results" {
		t.Fatalf("step1=%+v", steps[1])
	}
	if steps[0].Args["bundle_id"] != "com.atebits.Tweetie2" {
		t.Fatalf("args=%v", steps[0].Args)
	}
}

func TestExtractToolTraceLimitAndTruncation(t *testing.T) {
	// 5 assistant calls with results; limit 2 keeps the newest two.
	msgs := []model.Message{}
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		msgs = append(msgs,
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
				{ID: id, Type: "function", Function: model.FunctionCall{Name: "tool_" + id, Arguments: `{}`}},
			}},
			model.Message{Role: model.RoleTool, ToolCallID: id, Content: "r" + id},
		)
	}
	steps := extractToolTrace(msgs, 2)
	if len(steps) != 2 {
		t.Fatalf("steps=%d, want 2", len(steps))
	}
	if steps[0].Tool != "tool_d" || steps[1].Tool != "tool_e" {
		t.Fatalf("kept=%s,%s want d,e", steps[0].Tool, steps[1].Tool)
	}

	// Long result truncated.
	big := strings.Repeat("x", 5000)
	longMsgs := []model.Message{
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
			{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "t", Arguments: `{"path":"` + strings.Repeat("p", 5000) + `"}`}},
		}},
		{Role: model.RoleTool, ToolCallID: "c1", Content: big},
	}
	steps = extractToolTrace(longMsgs, 10)
	if len(steps[0].Result) > traceResultBudget+10 {
		t.Fatalf("result not truncated: %d bytes", len(steps[0].Result))
	}
	if len(steps[0].Args["path"].(string)) > traceArgsBudget+10 {
		t.Fatalf("arg not truncated: %d bytes", len(steps[0].Args["path"].(string)))
	}
}

func TestExtractToolTraceInvalidArgsKeptRaw(t *testing.T) {
	msgs := []model.Message{
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
			{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "t", Arguments: `{not json`}},
		}},
	}
	steps := extractToolTrace(msgs, 10)
	if len(steps) != 1 || steps[0].Args["_raw"] == "" {
		t.Fatalf("steps=%+v, want raw fallback", steps)
	}
}
