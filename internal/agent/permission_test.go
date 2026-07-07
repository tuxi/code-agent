package agent

import (
	"encoding/json"
	"testing"
)

// auditedFake is an AuditedApprover with a fixed verdict and reason, to drive
// r.approve's audit-emission path without a real auto-approver.
type auditedFake struct {
	v      Verdict
	reason string
}

func (f auditedFake) Approve(string, json.RawMessage) Verdict { return f.v }
func (f auditedFake) ApproveAudited(string, json.RawMessage) (Verdict, string) {
	return f.v, f.reason
}

// An auto-grant (non-empty reason) emits exactly one EventAutoApproved, stamped
// with the turn's correlation IDs and timestamp so it is retrievable per run.
func TestApprove_AuditedAutoGrantEmitsCorrelatedEvent(t *testing.T) {
	em := &capturingEmitter{}
	r := &Runner{Approver: auditedFake{v: VerdictAllow, reason: "workspace-internal write: a.go"}, Emitter: em}
	r.emitSessionID = "sess-1"
	r.emitTurnID = "turn-9"

	if r.approve("edit_file", json.RawMessage(`{"path":"a.go"}`)) != VerdictAllow {
		t.Fatal("expected approval")
	}
	if len(em.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(em.events))
	}
	ev := em.events[0]
	if ev.Kind != EventAutoApproved {
		t.Fatalf("kind = %s, want auto_approved", ev.Kind)
	}
	if ev.ToolName != "edit_file" || ev.Text == "" {
		t.Fatalf("event missing tool/reason: %+v", ev)
	}
	if ev.SessionID != "sess-1" || ev.TurnID != "turn-9" {
		t.Fatalf("event not correlated: session=%q turn=%q", ev.SessionID, ev.TurnID)
	}
	if ev.At.IsZero() {
		t.Fatal("event timestamp not stamped")
	}
}

// A human verdict or a denial (empty reason) is not an auto-grant → no audit event.
func TestApprove_HumanOrDenyEmitsNoAuditEvent(t *testing.T) {
	for _, f := range []auditedFake{
		{v: VerdictAllow, reason: ""}, // human approved — no auto-grant
		{v: VerdictDeny, reason: ""},  // denied
	} {
		em := &capturingEmitter{}
		r := &Runner{Approver: f, Emitter: em}
		r.approve("edit_file", json.RawMessage(`{"path":"a.go"}`))
		if len(em.events) != 0 {
			t.Fatalf("v=%v reason=%q: emitted %d events, want 0", f.v, f.reason, len(em.events))
		}
	}
}

// A plain Approver (no audit extension) takes the unchanged path: no audit events.
func TestApprove_PlainApproverUnchanged(t *testing.T) {
	em := &capturingEmitter{}
	r := &Runner{Approver: allowApprover{}, Emitter: em}
	if r.approve("edit_file", nil) != VerdictAllow {
		t.Fatal("allowApprover should approve")
	}
	if len(em.events) != 0 {
		t.Fatalf("plain approver should emit no audit events, got %d", len(em.events))
	}
}

// A nil Approver still fail-safe denies.
func TestApprove_NilApproverDenies(t *testing.T) {
	r := &Runner{}
	if r.approve("edit_file", nil) != VerdictDeny {
		t.Fatal("nil approver must fail-safe deny")
	}
}
