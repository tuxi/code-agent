package dialog

import (
	"testing"

	"code-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

func newPlanApprovalDialog(t *testing.T) (PlanApprovalDialogCmp, chan agent.PlanDecision) {
	t.Helper()
	reply := make(chan agent.PlanDecision, 1)
	d := NewPlanApprovalDialogCmp()
	d.SetPlan(PlanApprovalRequest{Plan: agent.Plan{ID: "p1", Title: "Refactor", Content: "# Refactor\nsteps"}, Reply: reply})
	return d, reply
}

func expectClosePlan(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("answering should return a command")
	}
	if _, ok := cmd().(ClosePlanApprovalMsg); !ok {
		t.Fatalf("expected ClosePlanApprovalMsg, got %T", cmd())
	}
}

func TestPlanApproveKeyApproves(t *testing.T) {
	d, reply := newPlanApprovalDialog(t)
	_, cmd := d.Update(keyRune('a'))
	if <-reply != agent.PlanApproved {
		t.Fatal("'a' should approve the plan")
	}
	expectClosePlan(t, cmd)
}

func TestPlanEnterApproves(t *testing.T) {
	d, reply := newPlanApprovalDialog(t)
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if <-reply != agent.PlanApproved {
		t.Fatal("enter should approve the plan")
	}
	expectClosePlan(t, cmd)
}

func TestPlanRejectKeyRejects(t *testing.T) {
	d, reply := newPlanApprovalDialog(t)
	_, cmd := d.Update(keyRune('r'))
	if <-reply != agent.PlanRejected {
		t.Fatal("'r' should reject the plan")
	}
	expectClosePlan(t, cmd)
}

func TestPlanEscRejects(t *testing.T) {
	d, reply := newPlanApprovalDialog(t)
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if <-reply != agent.PlanRejected {
		t.Fatal("esc should reject the plan")
	}
	expectClosePlan(t, cmd)
}
