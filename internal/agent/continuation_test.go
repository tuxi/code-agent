package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

func TestBackgroundContinuationPreservesBudgetAndCounters(t *testing.T) {
	store, sess := runtimeFixture(t)
	reg := tools.NewRegistry()
	probe := &outcomeTool{result: tools.ToolResult{Content: "waiting", RuntimeWaitFor: []string{"done"}, Usage: &tools.ToolUsage{ToolCallID: "stable-receipt", BillingUnits: 9}}}
	mustRegister(t, reg, probe)
	calls, starts, finishes := 0, 0, 0
	provider := runtimeProvider(func(req model.Request) (model.Response, error) {
		calls++
		if calls < 4 {
			return toolCallResp(probe.Name(), `{}`), nil
		}
		return model.Response{Content: "bounded summary", FinishReason: "stop"}, nil
	})
	var checkpoint json.RawMessage
	parker := func(_ context.Context, _ *session.Session, _ []string, data json.RawMessage) error {
		checkpoint = append(json.RawMessage(nil), data...)
		return nil
	}
	emitter := validationEmitter(func(e Event) {
		if e.Kind == EventTurnStarted {
			starts++
		}
		if e.Kind == EventTurnFinished {
			finishes++
		}
	})
	r := &Runner{Model: provider, Tools: reg, MaxSteps: 3, RuntimeInputs: store, ParkContinuation: parker, ReservedTurnID: "work", Emitter: emitter}
	res, err := r.RunTurn(context.Background(), sess, "start")
	if err != nil || !res.WaitingBackground || res.ExecutedToolCalls != 1 {
		t.Fatal(res, err)
	}
	for res.WaitingBackground {
		// A rebuilt runner's larger defaults must not replenish the budget.
		r = &Runner{Model: provider, Tools: reg, MaxSteps: 99, RuntimeInputs: store, ParkContinuation: parker, ReservedTurnID: "work", Emitter: emitter}
		res, err = r.ContinueBackground(context.Background(), sess, checkpoint)
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 4 || !res.HitStepLimit || res.ExecutedToolCalls != 3 || len(res.Steps) != 3 || starts != 1 || finishes != 1 {
		t.Fatalf("replenished budget: %+v calls=%d lifecycle=%d/%d", res, calls, starts, finishes)
	}
	if res.BillableToolCalls != 1 || res.ToolBillingUnits != 9 || res.BillingUnits != 9 {
		t.Fatal("receipt billed again after resume", res)
	}
	assertBalancedToolCalls(t, sess.Messages)
}

func TestBackgroundContinuationPreservesSearchBudget(t *testing.T) {
	store, sess := runtimeFixture(t)
	reg := tools.NewRegistry()
	search := &sequencedTool{name: "web_search", outs: []string{"search result"}}
	wait := &outcomeTool{result: tools.ToolResult{Content: "waiting", RuntimeWaitFor: []string{"done"}}}
	mustRegister(t, reg, search)
	mustRegister(t, reg, wait)
	first := toolCallResp(search.Name(), `{}`)
	first.ToolCalls = append(first.ToolCalls, toolCallResp(wait.Name(), `{}`).ToolCalls...)
	provider := &scriptedProvider{responses: []model.Response{first, toolCallResp(search.Name(), `{}`), {Content: "done", FinishReason: "stop"}}}
	var checkpoint json.RawMessage
	parker := func(_ context.Context, _ *session.Session, _ []string, data json.RawMessage) error {
		checkpoint = append(json.RawMessage(nil), data...)
		return nil
	}
	r := &Runner{Model: provider, Tools: reg, MaxSteps: 4, MaxWebSearches: 1, RuntimeInputs: store, ParkContinuation: parker, ReservedTurnID: "work"}
	result, err := r.RunTurn(context.Background(), sess, "start")
	if err != nil || !result.WaitingBackground {
		t.Fatal(result, err)
	}
	r = &Runner{Model: provider, Tools: reg, MaxSteps: 99, MaxWebSearches: 99, RuntimeInputs: store, ParkContinuation: parker, ReservedTurnID: "work"}
	result, err = r.ContinueBackground(context.Background(), sess, checkpoint)
	if err != nil || result.WaitingBackground || search.i != 1 || result.ExecutedToolCalls != 2 {
		t.Fatal("search budget refreshed", result, search.i, err)
	}
}
