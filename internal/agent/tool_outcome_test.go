package agent

import (
	"context"
	"encoding/json"
	"testing"

	"code-agent/internal/assetref"
	"code-agent/internal/observation"
	"code-agent/internal/tools"
)

type outcomeTool struct {
	result tools.ToolResult
	calls  int
}

func (*outcomeTool) Name() string                 { return "outcome_probe" }
func (*outcomeTool) Description() string          { return "probe" }
func (*outcomeTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (p *outcomeTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	p.calls++
	return p.result, nil
}

func TestBusinessFailureAgreesAcrossCountersStepsAndEvents(t *testing.T) {
	for _, observing := range []bool{false, true} {
		for _, kind := range []string{"blocked", "edit_conflict", "timeout", "canceled"} {
			probe := &outcomeTool{result: tools.FailureResult(kind, "operation did not complete")}
			reg := tools.NewRegistry()
			mustRegister(t, reg, probe)
			var finished Event
			r := &Runner{Tools: reg, Emitter: validationEmitter(func(e Event) {
				if e.Kind == EventToolFinished {
					finished = e
				}
			})}
			if observing {
				r.Observer = observation.DefaultObserver{}
			}
			var turn TurnResult
			var refs []assets.Ref
			searches := 0
			err := r.executeToolBatch(context.Background(), newSession(), &turn, toolCallResp(probe.Name(), `{}`).ToolCalls, reg, map[string]bool{probe.Name(): true}, &searches, &refs)
			if err != nil || turn.ExecutedToolCalls != 1 || turn.SucceededToolCalls != 0 || len(turn.Steps) != 1 {
				t.Fatalf("kind=%s result=%+v err=%v", kind, turn, err)
			}
			if finished.Failure != kind {
				t.Fatalf("event failure=%q want %q", finished.Failure, kind)
			}
			if kind == "canceled" {
				if finished.Err != "" || turn.Steps[0].Error != "" {
					t.Fatal("cancellation became an error")
				}
			} else if finished.Err == "" || turn.Steps[0].Error == "" {
				t.Fatal("failure reported success")
			}
		}
	}
}

func TestNestedDomainCannotExecuteUnadvertisedImplementation(t *testing.T) {
	probe := &outcomeTool{}
	r := &Runner{Tools: tools.NewRegistry()}
	if _, err := r.ExecuteNestedTool(context.Background(), "parent", "nested", probe, json.RawMessage(`{}`)); err == nil || probe.calls != 0 {
		t.Fatalf("domain bypass: calls=%d err=%v", probe.calls, err)
	}
}

func TestNestedUnavailableClientDoesNotEmitExecutionInstruction(t *testing.T) {
	r := &Runner{Tools: tools.NewRegistry(), Emitter: validationEmitter(func(e Event) {
		if e.Kind == EventToolStarted && e.Executor == "client" {
			t.Error("unavailable tool dispatched to device")
		}
	})}
	proxy := tools.NewClientProxyTool("capture", "device", tools.Object(nil).JSON())
	if _, err := r.ExecuteNestedTool(context.Background(), "parent", "nested", proxy, json.RawMessage(`{}`)); err == nil {
		t.Fatal("unavailable client tool accepted")
	}
}

type partialWriteTool struct{ outcomeTool }

func (*partialWriteTool) Name() string { return "edit_file" }

func TestPartialWriteReceiptSurvivesBusinessFailure(t *testing.T) {
	result := tools.FailureResult("runtime", "later operation failed")
	result.Wrote = []tools.FileWrite{{Path: "a.go", Added: 1, Status: tools.WriteModified}}
	probe := &partialWriteTool{outcomeTool{result: result}}
	reg := tools.NewRegistry()
	mustRegister(t, reg, probe)
	r := &Runner{Tools: reg}
	var turn TurnResult
	var refs []assets.Ref
	searches := 0
	if err := r.executeToolBatch(context.Background(), newSession(), &turn, toolCallResp(probe.Name(), `{}`).ToolCalls, reg, map[string]bool{probe.Name(): true}, &searches, &refs); err != nil {
		t.Fatal(err)
	}
	if turn.SucceededToolCalls != 0 || turn.DiffStat == nil || turn.DiffStat.AddedLines != 1 {
		t.Fatalf("partial write lost: %+v", turn)
	}
}

func TestClientBrokerFailureKindReachesFinishedEvent(t *testing.T) {
	for _, kind := range []string{"timeout", "transport"} {
		proxy := tools.NewClientProxyTool("device", "device", tools.Object(nil).JSON())
		reg := tools.NewRegistry()
		mustRegister(t, reg, proxy)
		var finished Event
		r := &Runner{Tools: reg, ClientWaiter: &immediateClientWaiter{result: ToolCallResult{IsError: true, Content: "device unavailable", Failure: &tools.ToolFailure{Kind: kind}}}, Emitter: validationEmitter(func(e Event) {
			if e.Kind == EventToolFinished {
				finished = e
			}
		})}
		var turn TurnResult
		var refs []assets.Ref
		searches := 0
		if err := r.executeToolBatch(context.Background(), newSession(), &turn, toolCallResp(proxy.Name(), `{}`).ToolCalls, reg, map[string]bool{proxy.Name(): true}, &searches, &refs); err != nil {
			t.Fatal(err)
		}
		if turn.SucceededToolCalls != 0 || finished.Failure != kind || finished.Err == "" {
			t.Fatalf("broker failure lost: %+v %+v", turn, finished)
		}
	}
}

type outcomeHook struct{ validationHook }

func (*outcomeHook) PostToolResult(_ context.Context, _ string, _ json.RawMessage, _ string, _ bool, output json.RawMessage) (string, bool, json.RawMessage, error) {
	return "rejected by result hook", true, output, nil
}

func TestResultHookErrorFlagIsCommitted(t *testing.T) {
	probe := &outcomeTool{result: tools.ToolResult{Content: "ok"}}
	reg := tools.NewRegistry()
	mustRegister(t, reg, probe)
	r := &Runner{Tools: reg, Hook: &outcomeHook{}}
	var turn TurnResult
	var refs []assets.Ref
	searches := 0
	if err := r.executeToolBatch(context.Background(), newSession(), &turn, toolCallResp(probe.Name(), `{}`).ToolCalls, reg, map[string]bool{probe.Name(): true}, &searches, &refs); err != nil {
		t.Fatal(err)
	}
	if turn.SucceededToolCalls != 0 || turn.Steps[0].Error == "" {
		t.Fatalf("ignored hook failure: %+v", turn)
	}
}
