package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/tools"
)

func TestPlanStateString(t *testing.T) {
	cases := map[PlanStatus]string{
		PlanStatusNone:      "none",
		PlanStatusPlanning:  "planning",
		PlanStatusProposing: "proposing",
		PlanStatusApproved:  "approved",
		PlanStatusRejected:  "rejected",
		PlanStatusExecuting: "executing",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("PlanStatus(%d).String() = %q, want %q", s, got, want)
		}
	}
}

// TestSetPlanStateEmitsAndNoOps: a real transition emits plan_state_changed
// carrying the new state; a redundant transition emits nothing.
func TestSetPlanStateEmitsAndNoOps(t *testing.T) {
	em := &capturingEmitter{}
	r := &Runner{Emitter: em}
	r.SetPlanState(PlanStatusPlanning)
	if ev, ok := em.first(EventPlanStateChanged); !ok || ev.PlanState != PlanStatusPlanning {
		t.Fatalf("expected plan_state_changed(planning), got %+v (ok=%v)", ev, ok)
	}
	n := len(em.events)
	r.SetPlanState(PlanStatusPlanning) // no-op
	if len(em.events) != n {
		t.Fatalf("redundant SetPlanState must not emit; events %d -> %d", n, len(em.events))
	}
	r.SetPlanState(PlanStatusExecuting)
	if len(em.events) != n+1 || em.events[n].PlanState != PlanStatusExecuting {
		t.Fatalf("expected one more plan_state_changed(executing), got %+v", em.events[n:])
	}
}

// TestBeginPlanningEmitsPlanStateChanged: entering plan mode (the enter_plan_mode
// path) emits plan_state_changed(planning).
func TestBeginPlanningEmitsPlanStateChanged(t *testing.T) {
	em := &capturingEmitter{}
	r := &Runner{Emitter: em}
	r.BeginPlanning("title")
	ev, ok := em.first(EventPlanStateChanged)
	if !ok || ev.PlanState != PlanStatusPlanning {
		t.Fatalf("expected plan_state_changed(planning), got %+v (ok=%v)", ev, ok)
	}
}

// TestEnterPlanModeToolEmitsStateChange drives the real enter_plan_mode tool.
func TestEnterPlanModeToolEmitsStateChange(t *testing.T) {
	em := &capturingEmitter{}
	ref := &RunnerRef{}
	r := &Runner{Emitter: em}
	ref.R = r
	tool := NewEnterPlanModeTool(ref)
	if _, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"title":"x"}`)); err != nil {
		t.Fatal(err)
	}
	ev, ok := em.first(EventPlanStateChanged)
	if !ok || ev.PlanState != PlanStatusPlanning {
		t.Fatalf("expected plan_state_changed(planning), got %+v (ok=%v)", ev, ok)
	}
}

type rejectPlanApprover struct{}

func (rejectPlanApprover) ApprovePlan(Plan) PlanDecision { return PlanRejected }

// TestProposePlanEmitsStateTransitions: propose drives planning -> proposing ->
// (approve) executing, emitting plan_state_changed on each real transition.
func TestProposePlanEmitsStateTransitions(t *testing.T) {
	root := t.TempDir()
	plansDir := filepath.Join(root, ".codeagent", "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plansDir, "p.md"), []byte("# Plan\n\nsteps"), 0o644); err != nil {
		t.Fatal(err)
	}

	em := &capturingEmitter{}
	ref := &RunnerRef{}
	r := &Runner{Emitter: em} // nil PlanApprover -> auto-approve (headless path)
	ref.R = r
	ent := NewEnterPlanModeTool(ref)
	prop := NewProposePlanTool(ref, plansDir)

	if _, err := ent.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(`{"title":"x"}`)); err != nil {
		t.Fatal(err)
	}
	in, err := json.Marshal(map[string]any{
		"plan_path":        ".codeagent/plans/p.md",
		"evidence_paths":   []string{"a.go"},
		"verification":     []string{"go test ./..."},
		"blocking_unknowns": []string{},
		"critic_summary":   "ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prop.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, in); err != nil {
		t.Fatal(err)
	}

	// Collect the plan_state_changed events in order.
	var states []PlanStatus
	for _, ev := range em.events {
		if ev.Kind == EventPlanStateChanged {
			states = append(states, ev.PlanState)
		}
	}
	want := []PlanStatus{PlanStatusPlanning, PlanStatusProposing, PlanStatusExecuting}
	if len(states) != len(want) {
		t.Fatalf("plan_state_changed sequence = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("plan_state_changed sequence = %v, want %v", states, want)
		}
	}
}

// TestProposePlanRejectedReEntersPlanning: rejection transitions back to
// planning and emits plan_state_changed(planning).
func TestProposePlanRejectedReEntersPlanning(t *testing.T) {
	root := t.TempDir()
	plansDir := filepath.Join(root, ".codeagent", "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plansDir, "p.md"), []byte("# Plan\n\nsteps"), 0o644); err != nil {
		t.Fatal(err)
	}

	em := &capturingEmitter{}
	ref := &RunnerRef{}
	r := &Runner{Emitter: em, PlanApprover: rejectPlanApprover{}}
	ref.R = r
	ent := NewEnterPlanModeTool(ref)
	prop := NewProposePlanTool(ref, plansDir)

	if _, err := ent.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(`{"title":"x"}`)); err != nil {
		t.Fatal(err)
	}
	in, err := json.Marshal(map[string]any{
		"plan_path":        ".codeagent/plans/p.md",
		"evidence_paths":   []string{"a.go"},
		"verification":     []string{"go test ./..."},
		"blocking_unknowns": []string{},
		"critic_summary":   "ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prop.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, in); err != nil {
		t.Fatal(err)
	}

	var states []PlanStatus
	for _, ev := range em.events {
		if ev.Kind == EventPlanStateChanged {
			states = append(states, ev.PlanState)
		}
	}
	want := []PlanStatus{PlanStatusPlanning, PlanStatusProposing, PlanStatusPlanning}
	if len(states) != len(want) {
		t.Fatalf("plan_state_changed sequence = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("plan_state_changed sequence = %v, want %v", states, want)
		}
	}
}
