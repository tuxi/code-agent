package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

type managedUsageTool struct{}

func (managedUsageTool) Name() string                 { return "managed_search" }
func (managedUsageTool) Description() string          { return "managed search" }
func (managedUsageTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (managedUsageTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{
		Content: "search result",
		Usage: &tools.ToolUsage{
			ToolCallID: "call_1", ToolName: "web_search", Provider: "tavily",
			Operation: "basic", ProviderCredits: 1, BillingUnits: 8000,
			FundingSource: "subscription", ReservationID: "res_1", PricingVersion: 2,
		},
	}, nil
}

func TestManagedToolUsageFlowsToToolFinishedEvent(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(managedUsageTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		{ToolCalls: []model.ToolCall{{ID: "call_1", Type: "function", Function: model.FunctionCall{Name: "managed_search", Arguments: `{}`}}}, FinishReason: "tool_calls", Usage: model.Usage{BillingUnits: 100}},
		{Content: "done", FinishReason: "stop", Usage: model.Usage{BillingUnits: 200}},
	}}
	emitter := &capturingEmitter{}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 3, Emitter: emitter}
	result, err := runner.RunTurn(context.Background(), newSession(), "search")
	if err != nil {
		t.Fatal(err)
	}
	event, ok := emitter.first(EventToolFinished)
	if !ok || event.ToolUsage == nil {
		t.Fatalf("tool_finished usage = %+v", event.ToolUsage)
	}
	if event.ToolUsage.BillingUnits != 8000 || event.ToolUsage.ProviderCredits != 1 {
		t.Fatalf("tool_finished usage = %+v", event.ToolUsage)
	}
	if result.ModelBillingUnits != 300 || result.ToolBillingUnits != 8000 || result.BillingUnits != 8300 {
		t.Fatalf("turn billing = model:%d tool:%d total:%d", result.ModelBillingUnits, result.ToolBillingUnits, result.BillingUnits)
	}
	if result.ExecutedToolCalls != 1 || result.SucceededToolCalls != 1 || result.BillableToolCalls != 1 {
		t.Fatalf("turn tool counters = executed:%d succeeded:%d billable:%d", result.ExecutedToolCalls, result.SucceededToolCalls, result.BillableToolCalls)
	}
	finished, ok := emitter.first(EventTurnFinished)
	if !ok || finished.BillingUnits != 8300 || finished.ModelBillingUnits != 300 || finished.ToolBillingUnits != 8000 {
		t.Fatalf("turn_finished billing = %+v", finished)
	}
}

type fatalManagedToolError struct{}

func (fatalManagedToolError) Error() string              { return "gateway auth failed" }
func (fatalManagedToolError) TurnFatalToolError() bool   { return true }
func (fatalManagedToolError) LifecycleErrorCode() string { return "auth_expired" }

type fatalManagedTool struct{}

func (fatalManagedTool) Name() string                 { return "managed_search" }
func (fatalManagedTool) Description() string          { return "managed search" }
func (fatalManagedTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (fatalManagedTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{}, fatalManagedToolError{}
}

func TestManagedToolFatalErrorStopsTurnWithBalancedHistory(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(fatalManagedTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{
		ToolCalls: []model.ToolCall{
			{ID: "call_1", Type: "function", Function: model.FunctionCall{Name: "managed_search", Arguments: `{}`}},
			{ID: "call_2", Type: "function", Function: model.FunctionCall{Name: "managed_search", Arguments: `{}`}},
		},
		FinishReason: "tool_calls",
	}}}
	sess := newSession()
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 3, MaxParallelTools: 1}
	_, err := runner.RunTurn(context.Background(), sess, "search")
	var fatal fatalManagedToolError
	if !errors.As(err, &fatal) {
		t.Fatalf("error = %T %v, want fatal managed tool error", err, err)
	}
	results := 0
	for _, message := range sess.Messages {
		if message.Role == model.RoleTool {
			results++
		}
	}
	if results != 2 {
		t.Fatalf("tool results = %d, want balanced history for both calls", results)
	}
}
