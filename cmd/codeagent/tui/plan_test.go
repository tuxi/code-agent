package tui

import (
	"encoding/json"
	"testing"

	"code-agent/cmd/codeagent/tui/components/dialog"
	"code-agent/internal/agent"
)

// The runner's blocking requests (approval / plan / askuser) each open their
// modal dialog and mark it blocking, so esc/ctrl+c answer the runner's request
// instead of merely closing the overlay — the guarantee the old approval overlay
// and plan toggle provided.
func TestApprovalMsgOpensBlockingPermissionDialog(t *testing.T) {
	m := newTestModel()
	reply := make(chan agent.Verdict, 1)
	tm, cmd := m.Update(approvalMsg{tool: "create_file", input: json.RawMessage(`{"path":"x"}`), reply: reply})
	m = asModel(t, tm)
	if !m.blocking || m.dialog == nil {
		t.Fatal("approvalMsg should open a blocking dialog")
	}
	if _, ok := m.dialog.(dialog.PermissionDialogCmp); !ok {
		t.Fatalf("approvalMsg should open the permission dialog, got %T", m.dialog)
	}
	if cmd == nil {
		t.Fatal("approvalMsg should re-issue the approval listener")
	}
}

func TestPlanApprovalMsgOpensPlanDialog(t *testing.T) {
	m := newTestModel()
	reply := make(chan agent.PlanDecision, 1)
	tm, cmd := m.Update(planApprovalMsg{plan: agent.Plan{ID: "p1", Title: "Refactor"}, reply: reply})
	m = asModel(t, tm)
	if !m.blocking || m.dialog == nil {
		t.Fatal("planApprovalMsg should open a blocking dialog")
	}
	if _, ok := m.dialog.(dialog.PlanApprovalDialogCmp); !ok {
		t.Fatalf("planApprovalMsg should open the plan dialog, got %T", m.dialog)
	}
	if cmd == nil {
		t.Fatal("planApprovalMsg should re-issue the plan listener")
	}
}

func TestAskUserMsgOpensAskUserDialog(t *testing.T) {
	m := newTestModel()
	reply := make(chan agent.AskUserAnswer, 1)
	tm, cmd := m.Update(askUserMsg{q: agent.AskUserQuestion{Question: "which approach?"}, reply: reply})
	m = asModel(t, tm)
	if !m.blocking || m.dialog == nil {
		t.Fatal("askUserMsg should open a blocking dialog")
	}
	if _, ok := m.dialog.(dialog.AskUserDialogCmp); !ok {
		t.Fatalf("askUserMsg should open the ask-user dialog, got %T", m.dialog)
	}
	if cmd == nil {
		t.Fatal("askUserMsg should re-issue the ask-user listener")
	}
}

// A dialog's answer flows back as its close message and the app closes it.
func TestDialogResponseClosesDialog(t *testing.T) {
	m := newTestModel()
	reply := make(chan agent.Verdict, 1)
	tm, _ := m.Update(approvalMsg{tool: "run_command", reply: reply})
	m = asModel(t, tm)
	if m.dialog == nil {
		t.Fatal("approvalMsg should open a dialog")
	}
	tm, _ = m.Update(dialog.PermissionResponseMsg{})
	m = asModel(t, tm)
	if m.dialog != nil || m.blocking {
		t.Fatal("a dialog response should close the dialog")
	}
}
