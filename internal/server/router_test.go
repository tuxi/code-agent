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

func (f *fakeCommands) Cancel() { f.canceled <- struct{}{} }

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
