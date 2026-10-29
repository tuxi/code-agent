package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/tools"
)

type validationProbe struct{ inspected, executed int }
type validationEmitter func(Event)

func (f validationEmitter) Emit(e Event)     { f(e) }
func (*validationProbe) Name() string        { return "probe_write" }
func (*validationProbe) Description() string { return "validation test" }
func (*validationProbe) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{"path": {Type: "string"}}, "path").JSON()
}
func (*validationProbe) SideEffects() bool                       { return true }
func (p *validationProbe) Inspect(json.RawMessage, string) error { p.inspected++; return nil }
func (p *validationProbe) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	p.executed++
	return tools.ToolResult{Content: "ok"}, nil
}

type validationApprover struct {
	calls   int
	verdict Verdict
}

func (a *validationApprover) Approve(string, json.RawMessage) Verdict { a.calls++; return a.verdict }

type validationHook struct{ pre int }

func (h *validationHook) PreToolUse(context.Context, string, json.RawMessage) error {
	h.pre++
	return nil
}
func (*validationHook) PostToolUse(context.Context, string, json.RawMessage, string) error {
	return nil
}
func (*validationHook) PostToolResult(_ context.Context, _ string, _ json.RawMessage, content string, isError bool, output json.RawMessage) (string, bool, json.RawMessage, error) {
	return content, isError, output, nil
}

func TestToolValidationPrecedesApprovalAndModelCanRepair(t *testing.T) {
	for _, parallel := range []int{1, 4} {
		probe := &validationProbe{}
		approver := &validationApprover{verdict: VerdictAllow}
		hook := &validationHook{}
		reg := tools.NewRegistry()
		mustRegister(t, reg, probe)
		provider := &scriptedProvider{responses: []model.Response{
			toolCallResp(probe.Name(), `{}`), toolCallResp(probe.Name(), `{"path":"a.go"}`), {Content: "done", FinishReason: "stop"},
		}}
		var events []Event
		r := &Runner{Model: provider, Tools: reg, MaxSteps: 4, MaxParallelTools: parallel, Approver: approver, Hook: hook, Emitter: validationEmitter(func(e Event) { events = append(events, e) })}
		res, err := r.RunTurn(context.Background(), newSession(), "repair arguments")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Steps) != 2 || !strings.Contains(res.Steps[0].Error, `"field":"path"`) || res.Steps[1].Error != "" {
			t.Fatalf("steps=%+v", res.Steps)
		}
		if approver.calls != 1 || hook.pre != 1 || probe.inspected != 1 || probe.executed != 1 {
			t.Fatalf("invalid call reached a gate: approval=%d hook=%d inspection=%d execution=%d", approver.calls, hook.pre, probe.inspected, probe.executed)
		}
		var outputs int
		for _, e := range events {
			if e.Kind == EventToolFinished && strings.Contains(string(e.Output), `"code":"invalid_tool_arguments"`) {
				outputs++
			}
		}
		if outputs != 1 {
			t.Fatalf("structured validation events=%d", outputs)
		}
		var toolMessages int
		for _, m := range provider.lastMessages {
			if m.Role == model.RoleTool {
				toolMessages++
			}
		}
		if toolMessages != 2 {
			t.Fatalf("history not balanced: %d results", toolMessages)
		}
	}
}

func TestToolValidationDoesNotGrantPermission(t *testing.T) {
	probe := &validationProbe{}
	reg := tools.NewRegistry()
	mustRegister(t, reg, probe)
	approver := &validationApprover{verdict: VerdictDeny}
	provider := &scriptedProvider{responses: []model.Response{toolCallResp(probe.Name(), `{"path":"../outside.go"}`), {Content: "done", FinishReason: "stop"}}}
	r := &Runner{Model: provider, Tools: reg, MaxSteps: 3, Approver: approver}
	if _, err := r.RunTurn(context.Background(), newSession(), "write"); err != nil {
		t.Fatal(err)
	}
	if approver.calls != 1 || probe.executed != 0 {
		t.Fatal("valid structure bypassed permission")
	}
}

func TestDispatcherMalformedArgumentsReturnStructuredResult(t *testing.T) {
	probe := &validationProbe{}
	reg := tools.NewRegistry()
	mustRegister(t, reg, probe)
	approver := &validationApprover{verdict: VerdictAllow}
	r := &Runner{Tools: reg, Approver: approver}
	var result TurnResult
	var refs []assets.Ref
	searches := 0
	// Exercise the dispatcher directly: provider history validation deliberately
	// rejects malformed wire envelopes before they can be persisted.
	err := r.executeToolBatch(context.Background(), newSession(), &result,
		toolCallResp(probe.Name(), `{"path":`).ToolCalls, reg, map[string]bool{probe.Name(): true}, &searches, &refs)
	if err != nil || len(result.Steps) != 1 || !strings.Contains(result.Steps[0].Error, `"code":"invalid_tool_arguments"`) || approver.calls != 0 {
		t.Fatalf("result=%+v err=%v approvals=%d", result, err, approver.calls)
	}
}

func TestToolValidationCoversClientAndNestedDispatch(t *testing.T) {
	schema := tools.Object(map[string]tools.Property{"path": {Type: "string"}}, "path").JSON()
	proxy := tools.NewClientProxyTool("client_write", "write", schema)
	waiter := &countingClientWaiter{}
	approver := &validationApprover{verdict: VerdictAllow}
	reg := tools.NewRegistry()
	mustRegister(t, reg, proxy)
	provider := &scriptedProvider{responses: []model.Response{toolCallResp(proxy.Name(), `{}`), {Content: "done", FinishReason: "stop"}}}
	r := &Runner{Model: provider, Tools: reg, ClientWaiter: waiter, Approver: approver, MaxSteps: 3}
	r.Emitter = validationEmitter(func(event Event) {
		if event.Kind == EventToolStarted && event.Executor == "client" {
			t.Error("invalid call emitted a client execution instruction")
		}
	})
	if _, err := r.RunTurn(context.Background(), newSession(), "write"); err != nil {
		t.Fatal(err)
	}
	if waiter.calls != 0 || approver.calls != 0 {
		t.Fatal("invalid client call reached approval or device")
	}
	for _, tool := range []tools.Tool{proxy, &validationProbe{}} {
		result, err := r.ExecuteNestedTool(context.Background(), "parent", "nested", tool, json.RawMessage(`{}`))
		if err == nil || !strings.Contains(string(result.Output), `"field":"path"`) {
			t.Fatalf("nested validation=%s %v", result.Output, err)
		}
	}
	if waiter.calls != 0 || approver.calls != 0 {
		t.Fatal("invalid nested call reached approval or device")
	}
}
