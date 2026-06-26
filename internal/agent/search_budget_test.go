package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// countingSearchTool stands in for web_search and records how often it actually ran.
type countingSearchTool struct{ ran int }

func (t *countingSearchTool) Name() string                 { return "web_search" }
func (t *countingSearchTool) Description() string          { return "search the web" }
func (t *countingSearchTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t *countingSearchTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	t.ran++
	return tools.ToolResult{Content: "results"}, nil
}

func searchCall(id string) model.Response {
	return model.Response{
		ToolCalls: []model.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: model.FunctionCall{Name: "web_search", Arguments: `{"query":"x"}`},
		}},
		FinishReason: "tool_calls",
	}
}

// TestWebSearchBudgetCapsThrash: a model that keeps issuing web_search is cut off
// at the budget, and told to stop, rather than searching unbounded.
func TestWebSearchBudgetCapsThrash(t *testing.T) {
	st := &countingSearchTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(st); err != nil {
		t.Fatalf("register: %v", err)
	}

	provider := &scriptedProvider{responses: []model.Response{
		searchCall("c1"), searchCall("c2"), searchCall("c3"), searchCall("c4"),
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 10, MaxWebSearches: 2}

	res, err := runner.RunTurn(context.Background(), newSession(), "search a lot")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if st.ran != 2 {
		t.Errorf("web_search ran %d times, want capped at 2", st.ran)
	}

	var sawBudgetMsg bool
	for _, s := range res.Steps {
		if strings.Contains(s.Observation, "Search budget reached") {
			sawBudgetMsg = true
		}
	}
	if !sawBudgetMsg {
		t.Error("expected a 'Search budget reached' observation once over budget")
	}
}

// TestWebSearchBudgetResetsPerTurn: the budget counts continuously within one
// turn and resets for the next — not a global lifetime cap.
func TestWebSearchBudgetResetsPerTurn(t *testing.T) {
	st := &countingSearchTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(st); err != nil {
		t.Fatalf("register: %v", err)
	}

	provider := &scriptedProvider{responses: []model.Response{
		searchCall("a1"), searchCall("a2"), {Content: "done", FinishReason: "stop"},
		searchCall("b1"), searchCall("b2"), {Content: "done2", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 10, MaxWebSearches: 2}
	sess := newSession()

	if _, err := runner.RunTurn(context.Background(), sess, "turn one"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunTurn(context.Background(), sess, "turn two"); err != nil {
		t.Fatal(err)
	}

	// 2 per turn × 2 turns = 4 proves the counter resets per turn rather than
	// capping globally at 2.
	if st.ran != 4 {
		t.Errorf("web_search ran %d times across two turns, want 4 (2/turn, reset between)", st.ran)
	}
}
