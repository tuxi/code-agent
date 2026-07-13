package server

import (
	"encoding/json"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/approve"
)

// waitApprovalID polls the sink for the approval_request frame and returns its id.
func waitApprovalID(t *testing.T, s *syncSink) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.count() >= 1 {
			var req map[string]any
			if err := json.Unmarshal(s.at(0), &req); err == nil && req["type"] == "approval_request" {
				if id, _ := req["id"].(string); id != "" {
					return id
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no approval_request frame was sent")
	return ""
}

func TestRemoteApproverResolveApproves(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(sink, time.Second, nil)

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("run_command", json.RawMessage(`{"command":"x"}`)) }()

	id := waitApprovalID(t, sink)
	if got := a.PendingCount(); got != 1 {
		t.Fatalf("pending before resolve=%d want 1", got)
	}
	a.Resolve(id, true)
	if got := a.PendingCount(); got != 0 {
		t.Fatalf("pending after accepted verdict=%d want 0", got)
	}
	// A duplicate verdict is ignored and cannot recreate attention.
	a.Resolve(id, false)

	select {
	case v := <-got:
		if v != agent.VerdictAllow {
			t.Error("Approve returned false after an approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after Resolve")
	}
}

func TestRemoteApproverResolveDenies(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(sink, time.Second, nil)

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("run_command", nil) }()

	a.Resolve(waitApprovalID(t, sink), false)

	if <-got == agent.VerdictAllow {
		t.Error("Approve returned true after a denial")
	}
}

// fakeGranter records the last GrantTool call.
type fakeGranter struct {
	tool  string
	scope int
}

func (g *fakeGranter) GrantTool(toolName string, scope approve.Scope) (string, error) {
	g.tool = toolName
	g.scope = int(scope)
	return toolName, nil
}

// A three-way "always" verdict approves the call AND persists a rule via the
// granter; "once" approves without granting.
func TestRemoteApproverResolveToolAlwaysGrants(t *testing.T) {
	sink := &syncSink{}
	g := &fakeGranter{}
	a := NewRemoteApprover(sink, time.Second, g)

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("mcp__github__list_issues", nil) }()

	a.ResolveTool(waitApprovalID(t, sink), true, true, 1 /* ScopeUser */)

	if <-got != agent.VerdictAllow {
		t.Error("Approve returned false after an 'always' approval")
	}
	if g.tool != "mcp__github__list_issues" || g.scope != 1 {
		t.Errorf("granter got tool=%q scope=%d, want the tool at scope user(1)", g.tool, g.scope)
	}
}

func TestRemoteApproverResolveToolOnceDoesNotGrant(t *testing.T) {
	sink := &syncSink{}
	g := &fakeGranter{}
	a := NewRemoteApprover(sink, time.Second, g)

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("mcp__github__list_issues", nil) }()

	a.ResolveTool(waitApprovalID(t, sink), true, false, 0)

	if <-got != agent.VerdictAllow {
		t.Error("Approve returned false after a 'once' approval")
	}
	if g.tool != "" {
		t.Errorf("'once' must not persist a rule, but granter saw %q", g.tool)
	}
}

func TestRemoteApproverTimeoutDenies(t *testing.T) {
	a := NewRemoteApprover(&syncSink{}, 20*time.Millisecond, nil)
	if a.Approve("x", nil) != agent.VerdictDeny {
		t.Error("Approve must deny when no response arrives before the deadline")
	}
}

// Zero timeout (the server default) means an approval waits indefinitely — an
// overnight turn parked on an approval must still be approvable the next
// morning, and the request frame carries no deadline_ms.
func TestRemoteApproverZeroTimeoutWaitsForVerdict(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(sink, 0, nil)

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("run_command", nil) }()

	id := waitApprovalID(t, sink)
	var req map[string]any
	if err := json.Unmarshal(sink.at(0), &req); err != nil {
		t.Fatalf("unmarshal request frame: %v", err)
	}
	if _, present := req["deadline_ms"]; present {
		t.Error("zero-timeout approval_request must not carry deadline_ms")
	}

	select {
	case <-got:
		t.Fatal("Approve returned without a verdict despite zero timeout")
	case <-time.After(100 * time.Millisecond):
	}

	a.Resolve(id, true)
	select {
	case v := <-got:
		if v != agent.VerdictAllow {
			t.Error("Approve returned false after an approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after Resolve")
	}
}

func TestRemoteApproverCloseDeniesPending(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(sink, 0, nil) // no deadline; rely on Close

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("run_command", nil) }()
	waitApprovalID(t, sink) // request sent => pending registered

	a.Close()

	select {
	case v := <-got:
		if v == agent.VerdictAllow {
			t.Error("a pending Approve must deny when the approver is closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after Close")
	}
}

func TestRemoteApproverClosedRejectsImmediately(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(sink, time.Second, nil)
	a.Close()

	if a.Approve("x", nil) != agent.VerdictDeny {
		t.Error("a closed approver must deny")
	}
	if sink.count() != 0 {
		t.Error("a closed approver must not send a request frame")
	}
}

func TestRemoteApproverDeniesOnSendError(t *testing.T) {
	// With a broken sink the request stays registered (send error is ignored).
	// The timeout should eventually deny.
	a := NewRemoteApprover(&errSink{failAt: 1}, 50*time.Millisecond, nil)
	start := time.Now()
	if a.Approve("x", nil) != agent.VerdictDeny {
		t.Error("Approve must deny when no response arrives before the deadline")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Error("Approve must not deny immediately on send error — the request should wait for the timeout")
	}
}

func TestRemoteApproverNilSinkDoesNotSend(t *testing.T) {
	// A nil sink means no client is connected. Approve should register the
	// request and block, not panic.
	a := NewRemoteApprover(nil, 50*time.Millisecond, nil)
	if a.Approve("x", nil) != agent.VerdictDeny {
		t.Error("Approve must deny on timeout when no sink is available")
	}
}

func TestRemoteApproverClearSinkDoesNotDeny(t *testing.T) {
	// ClearSink must not resolve pending requests — they stay registered and
	// can be re-sent when a new client connects.
	sink := &syncSink{}
	a := NewRemoteApprover(sink, 2*time.Second, nil)

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("run_command", nil) }()
	waitApprovalID(t, sink) // request sent to first sink

	a.ClearSink()

	// Pending request must still be alive.
	select {
	case <-got:
		t.Error("ClearSink must not deny pending requests")
	case <-time.After(100 * time.Millisecond):
		// OK
	}

	// Reconnect with a new sink — the pending request must be re-sent.
	newSink := &syncSink{}
	a.UpdateSink(newSink)
	id := waitApprovalID(t, newSink)
	a.Resolve(id, true)

	select {
	case v := <-got:
		if v != agent.VerdictAllow {
			t.Error("Approve returned false after ClearSink + UpdateSink + Resolve")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return")
	}
}

func TestRemoteApproverUpdateSinkResends(t *testing.T) {
	// After updating the sink, pending requests must be re-sent to the new sink.
	a := NewRemoteApprover(nil, 2*time.Second, nil) // no sink initially

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("run_command", json.RawMessage(`{"cmd":"ls"}`)) }()

	// Give Approve time to register the pending request.
	time.Sleep(20 * time.Millisecond)

	// Now connect a new client.
	newSink := &syncSink{}
	a.UpdateSink(newSink)

	// The pending request must have been re-sent.
	id := waitApprovalID(t, newSink)

	a.Resolve(id, true)

	select {
	case v := <-got:
		if v != agent.VerdictAllow {
			t.Error("Approve returned false after re-send + approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after re-send + Resolve")
	}
}

func TestRemoteApproverUnknownResolveIsNoop(t *testing.T) {
	a := NewRemoteApprover(&syncSink{}, time.Second, nil)
	a.Resolve("appr_missing", true) // must not panic
}
