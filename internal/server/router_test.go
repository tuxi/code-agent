package server

import (
	"context"
	"testing"
	"time"

	"code-agent/internal/agent"
)

type fakeCommands struct {
	text     chan string
	canceled chan struct{}
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{text: make(chan string, 1), canceled: make(chan struct{}, 1)}
}

func (f *fakeCommands) SendMessage(_ context.Context, text string) (agent.TurnResult, error) {
	f.text <- text
	return agent.TurnResult{}, nil
}

func (f *fakeCommands) Cancel()                             { f.canceled <- struct{}{} }
func (f *fakeCommands) RegisterTools([]agent.ClientToolDef) {}

type fakeResolver struct {
	id       string
	approved bool
	called   chan struct{}
}

func (f *fakeResolver) Resolve(id string, approved bool) {
	f.id = id
	f.approved = approved
	f.called <- struct{}{}
}

func TestRouterSendMessage(t *testing.T) {
	cmds := newFakeCommands()
	r := Router{Commands: cmds}
	r.Route(context.Background(), []byte(`{"type":"send_message","text":"hi"}`))
	select {
	case got := <-cmds.text:
		if got != "hi" {
			t.Errorf("text = %q, want hi", got)
		}
	case <-time.After(time.Second):
		t.Fatal("send_message did not reach the command target")
	}
}

func TestRouterCancelTurn(t *testing.T) {
	cmds := newFakeCommands()
	r := Router{Commands: cmds}
	r.Route(context.Background(), []byte(`{"type":"cancel_turn"}`))
	select {
	case <-cmds.canceled:
	case <-time.After(time.Second):
		t.Fatal("cancel_turn did not reach the command target")
	}
}

func TestRouterApprovalResponse(t *testing.T) {
	res := &fakeResolver{called: make(chan struct{}, 1)}
	r := Router{Approvals: res}
	r.Route(context.Background(), []byte(`{"type":"approval_response","id":"appr_9","approved":true}`))
	select {
	case <-res.called:
		if res.id != "appr_9" || !res.approved {
			t.Errorf("got id=%q approved=%v, want appr_9/true", res.id, res.approved)
		}
	case <-time.After(time.Second):
		t.Fatal("approval_response did not reach the resolver")
	}
}

type fakeToolResults struct {
	callID string
	result agent.ToolCallResult
	called chan struct{}
}

func (f *fakeToolResults) Deliver(callID string, result agent.ToolCallResult) {
	f.callID = callID
	f.result = result
	f.called <- struct{}{}
}

func TestRouterAgentInputText(t *testing.T) {
	cmds := newFakeCommands()
	r := Router{Commands: cmds}
	r.Route(context.Background(), []byte(`{"type":"agent_input","kind":"text","text":"hi via v1.1"}`))
	select {
	case got := <-cmds.text:
		if got != "hi via v1.1" {
			t.Errorf("text = %q, want hi via v1.1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("agent_input text did not reach the command target")
	}
}

func TestRouterAgentInputCommandCancel(t *testing.T) {
	cmds := newFakeCommands()
	r := Router{Commands: cmds}
	r.Route(context.Background(), []byte(`{"type":"agent_input","kind":"command","text":"cancel"}`))
	select {
	case <-cmds.canceled:
	case <-time.After(time.Second):
		t.Fatal("agent_input command cancel did not reach the command target")
	}
}

func TestRouterAgentInputToolResult(t *testing.T) {
	tr := &fakeToolResults{called: make(chan struct{}, 1)}
	r := Router{ToolResults: tr}
	r.Route(context.Background(), []byte(`{"type":"agent_input","kind":"tool_result","tool_result":{"tool_use_id":"call_99","subtype":"result","content":"done","is_error":false}}`))
	select {
	case <-tr.called:
		if tr.callID != "call_99" {
			t.Errorf("callID = %q, want call_99", tr.callID)
		}
		if tr.result.Content != "done" || tr.result.IsError {
			t.Errorf("result = %+v, want content=done isError=false", tr.result)
		}
	case <-time.After(time.Second):
		t.Fatal("agent_input tool_result did not reach the tool result resolver")
	}
}

func TestRouterAgentInputSystemIsNoop(t *testing.T) {
	cmds := newFakeCommands()
	r := Router{Commands: cmds}
	// system kind is a stub in v1.1 — must not panic and must not trigger commands.
	r.Route(context.Background(), []byte(`{"type":"agent_input","kind":"system","command":"patch_context","command_key":"project_rules","command_value":"x"}`))
	select {
	case <-cmds.text:
		t.Error("unexpected SendMessage from agent_input system")
	case <-cmds.canceled:
		t.Error("unexpected Cancel from agent_input system")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRouterRegisterTools(t *testing.T) {
	cmds := newFakeCommands()
	r := Router{Commands: cmds}
	r.Route(context.Background(), []byte(`{"type":"register_tools","tools":[{"name":"get_device_info","description":"获取设备信息","input_schema":{}}]}`))
	// register_tools is fire-and-forget, no response — just verify no panic.
}

func TestRouterIgnoresUnknownMalformedAndNilTargets(t *testing.T) {
	cmds := newFakeCommands()
	res := &fakeResolver{called: make(chan struct{}, 1)}
	r := Router{Commands: cmds, Approvals: res}

	r.Route(context.Background(), []byte(`{"type":"who_knows"}`)) // unknown type
	r.Route(context.Background(), []byte(`not json`))             // malformed

	// nil targets must not panic.
	Router{}.Route(context.Background(), []byte(`{"type":"send_message","text":"x"}`))
	Router{}.Route(context.Background(), []byte(`{"type":"cancel_turn"}`))
	Router{}.Route(context.Background(), []byte(`{"type":"approval_response","id":"a"}`))
	Router{}.Route(context.Background(), []byte(`{"type":"agent_input","kind":"text","text":"x"}`))
	Router{}.Route(context.Background(), []byte(`{"type":"agent_input","kind":"tool_result","tool_result":{"tool_use_id":"x","subtype":"result","content":"x"}}`))
	Router{}.Route(context.Background(), []byte(`{"type":"agent_input","kind":"command","text":"cancel"}`))
	Router{}.Route(context.Background(), []byte(`{"type":"agent_input","kind":"system"}`))
	Router{}.Route(context.Background(), []byte(`{"type":"register_tools","tools":[]}`))

	select {
	case <-cmds.text:
		t.Error("unexpected SendMessage")
	case <-cmds.canceled:
		t.Error("unexpected Cancel")
	case <-res.called:
		t.Error("unexpected Resolve")
	case <-time.After(50 * time.Millisecond):
	}
}
