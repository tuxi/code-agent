package server

import (
	"encoding/json"
	"testing"
	"time"
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
	a := NewRemoteApprover(sink, time.Second)

	got := make(chan bool, 1)
	go func() { got <- a.Approve("run_command", json.RawMessage(`{"command":"x"}`)) }()

	a.Resolve(waitApprovalID(t, sink), true)

	select {
	case v := <-got:
		if !v {
			t.Error("Approve returned false after an approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after Resolve")
	}
}

func TestRemoteApproverResolveDenies(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(sink, time.Second)

	got := make(chan bool, 1)
	go func() { got <- a.Approve("run_command", nil) }()

	a.Resolve(waitApprovalID(t, sink), false)

	if <-got {
		t.Error("Approve returned true after a denial")
	}
}

func TestRemoteApproverTimeoutDenies(t *testing.T) {
	a := NewRemoteApprover(&syncSink{}, 20*time.Millisecond)
	if a.Approve("x", nil) {
		t.Error("Approve must deny when no response arrives before the deadline")
	}
}

func TestRemoteApproverCloseDeniesPending(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(sink, 0) // no deadline; rely on Close

	got := make(chan bool, 1)
	go func() { got <- a.Approve("run_command", nil) }()
	waitApprovalID(t, sink) // request sent => pending registered

	a.Close()

	select {
	case v := <-got:
		if v {
			t.Error("a pending Approve must deny when the approver is closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after Close")
	}
}

func TestRemoteApproverClosedRejectsImmediately(t *testing.T) {
	sink := &syncSink{}
	a := NewRemoteApprover(sink, time.Second)
	a.Close()

	if a.Approve("x", nil) {
		t.Error("a closed approver must deny")
	}
	if sink.count() != 0 {
		t.Error("a closed approver must not send a request frame")
	}
}

func TestRemoteApproverDeniesOnSendError(t *testing.T) {
	a := NewRemoteApprover(&errSink{failAt: 1}, time.Second)
	if a.Approve("x", nil) {
		t.Error("Approve must deny when the request frame cannot be sent")
	}
}

func TestRemoteApproverUnknownResolveIsNoop(t *testing.T) {
	a := NewRemoteApprover(&syncSink{}, time.Second)
	a.Resolve("appr_missing", true) // must not panic
}
