package agent

import (
	"context"
	"encoding/json"
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
func (t readTool) Execute(context.Context, json.RawMessage) (tools.ToolResult, error) {
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
	runner := &Runner{Model: provider, Tools: full, PlanTools: planTools, PlanMode: true, MaxSteps: 5}
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
		if strings.Contains(m.Content, "PLAN MODE") {
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
	runner := &Runner{Model: provider, Tools: full, PlanTools: planTools, PlanMode: true, Approver: allowApprover{}, MaxSteps: 5}
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
