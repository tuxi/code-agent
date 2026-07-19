package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// readTool is a read-only fake (no SideEffects marker).
type readTool struct{ name string }

func (t readTool) Name() string                 { return t.name }
func (t readTool) Description() string          { return "" }
func (t readTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t readTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{}, nil
}

func TestPlanModeRestrictsToolsAndNudges(t *testing.T) {
	full := tools.NewRegistry()
	_ = full.Register(&recordingTool{}) // "danger" (side-effecting)
	_ = full.Register(readTool{"read_file"})
	planTools := tools.Subset(full, "read_file")

	provider := &scriptedProvider{responses: []model.Response{
		{Content: "here is the plan", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: full, PlanTools: planTools, PlanState: PlanStatusPlanning, MaxSteps: 5}
	if _, err := runner.RunTurn(context.Background(), newSession(), "plan it"); err != nil {
		t.Fatal(err)
	}

	// The write tool is not advertised in plan mode; the read tool is.
	var advertised []string
	for _, td := range provider.lastTools {
		advertised = append(advertised, td.Function.Name)
		if td.Function.Name == "danger" {
			t.Fatal("plan mode must not advertise a side-effecting tool")
		}
	}
	if len(advertised) != 1 || advertised[0] != "read_file" {
		t.Fatalf("plan mode toolset = %v, want [read_file]", advertised)
	}

	// The plan-mode reminder reached the model.
	var nudged bool
	for _, m := range provider.lastMessages {
		if strings.Contains(m.Content, "plan mode") {
			nudged = true
		}
	}
	if !nudged {
		t.Fatal("plan-mode reminder was not injected")
	}
}

func TestPlanModeBlocksWriteToolExecution(t *testing.T) {
	rt := &recordingTool{} // "danger"
	full := tools.NewRegistry()
	_ = full.Register(rt)
	planTools := tools.NewRegistry() // danger is NOT in the plan toolset

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("danger", "{}"), // the model tries to use a write tool
		{Content: "plan", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: full, PlanTools: planTools, PlanState: PlanStatusPlanning, Approver: allowApprover{}, MaxSteps: 5}
	if _, err := runner.RunTurn(context.Background(), newSession(), "plan it"); err != nil {
		t.Fatal(err)
	}
	if rt.ran {
		t.Fatal("plan mode must not execute a tool absent from PlanTools (even if the model calls it)")
	}
}

func TestPlanModeOffRunsApprovedTool(t *testing.T) {
	// Sanity: with plan mode off, the same tool executes once approved.
	rt := &recordingTool{}
	full := tools.NewRegistry()
	_ = full.Register(rt)
	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("danger", "{}"),
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: full, Approver: allowApprover{}, MaxSteps: 5}
	if _, err := runner.RunTurn(context.Background(), newSession(), "go"); err != nil {
		t.Fatal(err)
	}
	if !rt.ran {
		t.Fatal("without plan mode, an approved tool should run")
	}
}

// TestEnterPlanModeTransitionsState verifies that calling enter_plan_mode sets
// PlanState to Planning and switches the toolset.
func TestEnterPlanModeTransitionsState(t *testing.T) {
	dir := t.TempDir()

	full := tools.NewRegistry()
	_ = full.Register(readTool{"read_file"})
	_ = full.Register(&recordingTool{}) // "danger"

	// Register plan tools FIRST, then create the plan-mode subset.
	ref := &RunnerRef{}
	_ = full.Register(NewEnterPlanModeTool(ref))
	_ = full.Register(NewProposePlanTool(ref, dir))
	planTools := tools.Subset(full, "read_file", "enter_plan_mode", "propose_plan")

	// Model calls enter_plan_mode, then finishes.
	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("enter_plan_mode", `{"title":"Test Plan"}`),
		{Content: "Here is the plan...", FinishReason: "stop"},
	}}
	runner := &Runner{
		Model:         provider,
		Tools:         full,
		PlanTools:     planTools,
		MaxSteps:      5,
		WorkspaceRoot: dir,
	}
	ref.R = runner

	res, err := runner.RunTurn(context.Background(), newSession(), "plan this")
	if err != nil {
		t.Fatal(err)
	}
	if runner.PlanState != PlanStatusPlanning {
		t.Fatalf("PlanState should be Planning after enter_plan_mode, got %v", runner.PlanState)
	}
	if runner.lastAssistantText != "Here is the plan..." {
		t.Fatalf("lastAssistantText not captured, got %q", runner.lastAssistantText)
	}
	if res.Final != "Here is the plan..." {
		t.Fatalf("unexpected final answer: %q", res.Final)
	}
}

// TestProposePlanRejectedWithoutPlanning verifies propose_plan errors when
// called outside the planning state.
func TestProposePlanRejectedWithoutPlanning(t *testing.T) {
	full := tools.NewRegistry()
	ref := &RunnerRef{}
	_ = full.Register(NewProposePlanTool(ref, ""))

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("propose_plan", `{}`),
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: full, MaxSteps: 5}
	ref.R = runner

	_, err := runner.RunTurn(context.Background(), newSession(), "propose without entering")
	if err != nil {
		t.Fatal(err)
	}
	// propose_plan should have errored, not changed state.
	if runner.PlanState != PlanStatusNone {
		t.Fatalf("PlanState should stay None, got %v", runner.PlanState)
	}
}

// TestProposePlanApprovesAndTransitions tests the full flow:
// enter_plan_mode → produce plan → propose_plan → approved → executing.
func TestProposePlanApprovesAndTransitions(t *testing.T) {
	dir := t.TempDir()

	full := tools.NewRegistry()
	_ = full.Register(readTool{"read_file"})

	// Register plan tools FIRST, then create the plan-mode subset.
	ref := &RunnerRef{}
	_ = full.Register(NewEnterPlanModeTool(ref))
	_ = full.Register(NewProposePlanTool(ref, dir))
	planTools := tools.Subset(full, "read_file", "enter_plan_mode", "propose_plan")

	provider := &scriptedProvider{responses: []model.Response{
		toolCallResp("enter_plan_mode", `{"title":"Add Auth"}`),
		{Content: "# Plan\n\n1. Add login\n2. Add middleware", FinishReason: "stop"},
	}}
	runner := &Runner{
		Model:         provider,
		Tools:         full,
		PlanTools:     planTools,
		MaxSteps:      5,
		WorkspaceRoot: dir,
	}
	ref.R = runner

	_, err := runner.RunTurn(context.Background(), newSession(), "plan auth feature")
	if err != nil {
		t.Fatal(err)
	}
	if runner.PlanState != PlanStatusPlanning {
		t.Fatalf("expected Planning state, got %v", runner.PlanState)
	}
	if runner.lastAssistantText != "# Plan\n\n1. Add login\n2. Add middleware" {
		t.Fatalf("unexpected lastAssistantText: %q", runner.lastAssistantText)
	}

	// Second turn: propose the plan (no PlanApprover → auto-approve).
	provider2 := &scriptedProvider{responses: []model.Response{
		toolCallResp("propose_plan", `{"allowed_prompts":["run tests"]}`),
		{Content: "implementing now", FinishReason: "stop"},
	}}
	runner.Model = provider2
	_, err = runner.RunTurn(context.Background(), newSession(), "propose it")
	if err != nil {
		t.Fatal(err)
	}
	// With no PlanApprover, auto-approve → Executing.
	if runner.PlanState != PlanStatusExecuting {
		t.Fatalf("expected Executing state after approve, got %v", runner.PlanState)
	}
}

func TestProposePlanUsesCanonicalPlanFile(t *testing.T) {
	root := t.TempDir()
	plansDir := filepath.Join(root, ".codeagent", "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const planBody = "# Canonical Plan\n\n1. Change the server\n2. Update the client contract\n"
	planFile := filepath.Join(plansDir, "canonical.md")
	if err := os.WriteFile(planFile, []byte(planBody), 0o644); err != nil {
		t.Fatal(err)
	}

	ref := &RunnerRef{}
	runner := &Runner{
		PlanState:         PlanStatusPlanning,
		WorkspaceRoot:     root,
		planTitle:         "Canonical plan",
		lastAssistantText: "The plan is ready at .codeagent/plans/canonical.md.",
	}
	ref.R = runner
	tool := NewProposePlanTool(ref, plansDir)

	result, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root},
		json.RawMessage(`{"plan_path":".codeagent/plans/canonical.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	if runner.PlanState != PlanStatusExecuting {
		t.Fatalf("expected Executing state, got %v", runner.PlanState)
	}
	if runner.activePlan == nil {
		t.Fatal("active plan was not populated")
	}
	if runner.activePlan.Content != planBody {
		t.Fatalf("approval content = %q, want canonical file content", runner.activePlan.Content)
	}
	canonicalPlanFile, err := filepath.EvalSymlinks(planFile)
	if err != nil {
		t.Fatal(err)
	}
	if runner.activePlan.FilePath != canonicalPlanFile {
		t.Fatalf("file path = %q, want %q", runner.activePlan.FilePath, canonicalPlanFile)
	}
	if runner.activePlan.WorkspaceRelativePath != ".codeagent/plans/canonical.md" {
		t.Fatalf("relative path = %q", runner.activePlan.WorkspaceRelativePath)
	}
	if !strings.Contains(result.Content, canonicalPlanFile) {
		t.Fatalf("approval result does not reference canonical file: %q", result.Content)
	}
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "canonical.md" {
		t.Fatalf("propose_plan created a duplicate plan file: %v", entries)
	}
}

func TestProposePlanRejectsPathOutsidePlansDirectory(t *testing.T) {
	root := t.TempDir()
	plansDir := filepath.Join(root, ".codeagent", "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("# Not a plan directory file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ref := &RunnerRef{}
	runner := &Runner{PlanState: PlanStatusPlanning, WorkspaceRoot: root}
	ref.R = runner
	tool := NewProposePlanTool(ref, plansDir)

	_, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root},
		json.RawMessage(`{"plan_path":"outside.md"}`))
	if err == nil || !strings.Contains(err.Error(), "inside .codeagent/plans") {
		t.Fatalf("expected plans-directory rejection, got %v", err)
	}
	if runner.PlanState != PlanStatusPlanning {
		t.Fatalf("invalid plan changed state to %v", runner.PlanState)
	}
}
