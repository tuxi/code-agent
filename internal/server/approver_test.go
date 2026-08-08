package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/approve"
)

// waitAskUserFrame polls the sink for an ask_user_request frame and returns its
// decoded form.
func waitAskUserFrame(t *testing.T, s *syncSink) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for i := 0; i < s.count(); i++ {
			var req map[string]any
			if err := json.Unmarshal(s.at(i), &req); err == nil && req["type"] == "ask_user_request" {
				return req
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no ask_user_request frame was sent")
	return nil
}

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
	a := NewRemoteApprover(time.Second, nil)
	a.AddSink(1, sink)

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
	a := NewRemoteApprover(time.Second, nil)
	a.AddSink(1, sink)

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
	a := NewRemoteApprover(time.Second, g)
	a.AddSink(1, sink)

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
	a := NewRemoteApprover(time.Second, g)
	a.AddSink(1, sink)

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
	a := NewRemoteApprover(20*time.Millisecond, nil)
	a.AddSink(1, &syncSink{})
	if a.Approve("x", nil) != agent.VerdictDeny {
		t.Error("Approve must deny when no response arrives before the deadline")
	}
}

// Zero timeout (the server default) means an approval waits indefinitely — an
// overnight turn parked on an approval must still be approvable the next
// morning, and the request frame carries no deadline_ms.
func TestRemoteApproverZeroTimeoutWaitsForVerdict(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(0, nil)
	a.AddSink(1, sink)

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
	a := NewRemoteApprover(0, nil) // no deadline; rely on Close
	a.AddSink(1, sink)

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
	a := NewRemoteApprover(time.Second, nil)
	a.AddSink(1, sink)
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
	a := NewRemoteApprover(50*time.Millisecond, nil)
	a.AddSink(1, &errSink{failAt: 1})
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
	a := NewRemoteApprover(50*time.Millisecond, nil)
	if a.Approve("x", nil) != agent.VerdictDeny {
		t.Error("Approve must deny on timeout when no sink is available")
	}
}

func TestRemoteApproverClearSinkDoesNotDeny(t *testing.T) {
	// ClearSink must not resolve pending requests — they stay registered and
	// can be re-sent when a new client connects.
	sink := &syncSink{}
	a := NewRemoteApprover(2*time.Second, nil)
	a.AddSink(1, sink)

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
	a.AddSink(2, newSink)
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
	a := NewRemoteApprover(2*time.Second, nil) // no sink initially

	got := make(chan agent.Verdict, 1)
	go func() { got <- a.Approve("run_command", json.RawMessage(`{"cmd":"ls"}`)) }()

	// Give Approve time to register the pending request.
	time.Sleep(20 * time.Millisecond)

	// Now connect a new client.
	newSink := &syncSink{}
	a.AddSink(2, newSink)

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
	a := NewRemoteApprover(time.Second, nil)
	a.AddSink(1, &syncSink{})
	a.Resolve("appr_missing", true) // must not panic
}

func TestRemoteApproverAskUserConnectedNoDeadline(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(time.Second, nil)
	a.AddSink(1, sink)

	got := make(chan agent.AskUserAnswer, 1)
	errCh := make(chan error, 1)
	go func() {
		ans, err := a.AskUser(agent.AskUserQuestion{Question: "q", Options: []agent.AskOption{{Label: "a"}, {Label: "b"}}})
		errCh <- err
		got <- ans
	}()

	frame := waitAskUserFrame(t, sink)
	if _, present := frame["deadline_ms"]; present {
		t.Error("connected ask_user_request must not carry deadline_ms (no deadline)")
	}
	id, _ := frame["id"].(string)

	a.ResolveAskUser(id, agent.AskUserAnswer{Selected: []string{"a"}})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("AskUser error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskUser did not return after ResolveAskUser")
	}
	if ans := <-got; len(ans.Selected) != 1 || ans.Selected[0] != "a" {
		t.Fatalf("answer = %+v, want selected=[a]", ans)
	}
}

func TestRemoteApproverAskUserHeadlessTimesOut(t *testing.T) {
	a := NewRemoteApprover(time.Second, nil)
	a.askUserTimeout = 50 * time.Millisecond

	_, err := a.AskUser(agent.AskUserQuestion{Question: "q", Options: []agent.AskOption{{Label: "a"}, {Label: "b"}}})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("headless AskUser error = %v, want timeout", err)
	}
}

func TestRemoteApproverAskUserGracePeriodExpires(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(time.Second, nil)
	a.askUserPoll = 10 * time.Millisecond
	a.askUserGracePeriod = 50 * time.Millisecond
	a.AddSink(1, sink)

	done := make(chan error, 1)
	go func() {
		_, err := a.AskUser(agent.AskUserQuestion{Question: "q", Options: []agent.AskOption{{Label: "a"}, {Label: "b"}}})
		done <- err
	}()
	waitAskUserFrame(t, sink)

	// The client disconnects and never reconnects — the grace period must
	// expire instead of hanging the turn forever.
	a.RemoveSink(1)

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "grace period") {
			t.Fatalf("AskUser error = %v, want grace-period expiry", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskUser did not return after grace period expired")
	}
}

func TestRemoteApproverAskUserReconnectWithinGraceResumes(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(time.Second, nil)
	a.askUserPoll = 10 * time.Millisecond
	a.askUserGracePeriod = 2 * time.Second
	a.AddSink(1, sink)

	done := make(chan error, 1)
	go func() {
		_, err := a.AskUser(agent.AskUserQuestion{Question: "q", Options: []agent.AskOption{{Label: "a"}, {Label: "b"}}})
		done <- err
	}()
	waitAskUserFrame(t, sink)

	// Disconnect, then reconnect within the grace period — the pending ask is
	// re-sent and can still be answered.
	a.RemoveSink(1)
	time.Sleep(30 * time.Millisecond) // let the poll notice the disconnect and start grace
	a.AddSink(2, sink)

	frame := waitAskUserFrame(t, sink)
	id, _ := frame["id"].(string)
	a.ResolveAskUser(id, agent.AskUserAnswer{Selected: []string{"b"}})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AskUser error after reconnect = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskUser did not return after reconnect + answer")
	}
}

func TestRemoteApproverAskUserResendKeepsFrozenDeadline(t *testing.T) {
	// A headless ask freezes its fallback deadline; when a client connects
	// later and AddSink re-sends it, the frame must carry the same deadline_ms
	// the original would have — not a fresh recomputation.
	a := NewRemoteApprover(time.Second, nil)
	a.askUserTimeout = 42 * time.Second

	done := make(chan error, 1)
	go func() {
		_, err := a.AskUser(agent.AskUserQuestion{Question: "q", Options: []agent.AskOption{{Label: "a"}, {Label: "b"}}})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond) // let AskUser register the pending ask

	sink := &syncSink{}
	a.AddSink(1, sink)

	frame := waitAskUserFrame(t, sink)
	got, ok := frame["deadline_ms"].(float64)
	if !ok || int64(got) != 42*1000 {
		t.Fatalf("re-sent deadline_ms = %v (present=%v), want 42000", got, ok)
	}
	id, _ := frame["id"].(string)
	a.ResolveAskUser(id, agent.AskUserAnswer{Selected: []string{"a"}})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AskUser error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskUser did not return after resolve")
	}
}

func TestRemoteApproverCloseResolvesBlockedAskUser(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(time.Second, nil)
	a.AddSink(1, sink)

	done := make(chan error, 1)
	go func() {
		_, err := a.AskUser(agent.AskUserQuestion{Question: "q", Options: []agent.AskOption{{Label: "a"}, {Label: "b"}}})
		done <- err
	}()
	waitAskUserFrame(t, sink)

	a.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("AskUser returned nil error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskUser did not return after Close")
	}
}
