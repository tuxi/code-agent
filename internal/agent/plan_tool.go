package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"code-agent/internal/tools"
)

func newPlanID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "plan_" + hex.EncodeToString(b[:])
}

// enterPlanModeTool lets the model voluntarily enter plan mode for complex tasks.
// It is registered in the main toolset (visible to the model) and dereferences
// its RunnerRef lazily at Execute time — the Runner is set after construction.
// The loop does NOT need special handling: the tool's Execute changes planState,
// and the next loop iteration recomputes activeTools.
type enterPlanModeTool struct {
	ref *RunnerRef
}

func (t *enterPlanModeTool) Name() string { return "enter_plan_mode" }

func (t *enterPlanModeTool) Description() string {
	return "Switch to plan mode for complex tasks. " +
		"Use this BEFORE making changes when the task involves: " +
		"new feature implementation, multiple valid approaches, " +
		"architectural decisions, modifying existing behavior, " +
		"multi-file changes (more than 2-3 files), or unclear requirements. " +
		"In plan mode you can read, search, and write plan files, " +
		"but you CANNOT edit project files or run commands — " +
		"this ensures you research and design before implementing. " +
		"When you have a complete plan, call propose_plan to get user approval."
}

func (t *enterPlanModeTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"title": {Type: "string", Description: "Brief title for the implementation plan."},
	}).JSON()
}

func (t *enterPlanModeTool) Execute(_ context.Context, _ tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	r := t.ref.R
	if r == nil {
		return tools.ToolResult{}, fmt.Errorf("enter_plan_mode: runner not wired")
	}

	// Already in a plan state: no-op with a gentle reminder.
	if r.PlanState != PlanStatusNone && r.PlanState != PlanStatusExecuting {
		return tools.ToolResult{
			Content: "Already in plan mode. Continue researching and call propose_plan when ready.",
		}, nil
	}

	var in struct{ Title string }
	_ = json.Unmarshal(input, &in)

	r.PlanState = PlanStatusPlanning
	r.planTitle = in.Title
	return tools.ToolResult{
		Content: "Plan mode activated. Research the task thoroughly, then produce a " +
			"concrete implementation plan. You may write the plan to .codeagent/plans/ " +
			"for the user to review. When the plan is complete, call propose_plan to " +
			"submit it for user approval.",
	}, nil
}

// proposePlanTool presents the model's plan to the user for approval. It is
// callable in plan mode. The model writes its plan as the text answer BEFORE
// calling this tool; Execute reads it from Runner.lastThinking.
type proposePlanTool struct {
	ref      *RunnerRef
	plansDir string // absolute path; set at construction or via SetPlansDir
}

// SetPlansDir sets the plans directory if not known at construction time.
func (t *proposePlanTool) SetPlansDir(dir string) { t.plansDir = dir }

func (t *proposePlanTool) Name() string { return "propose_plan" }

func (t *proposePlanTool) Description() string {
	return "Propose your implementation plan for user approval. " +
		"Call this AFTER you have researched and written a complete plan. " +
		"Write the plan as your text answer, then call this tool. " +
		"The user will review the plan and approve or reject it. " +
		"If approved you may proceed to implement with full tool access. " +
		"If rejected you must revise the plan."
}

func (t *proposePlanTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"allowed_prompts": {
			Type: "array",
			Description: "Categories of tool calls the implementation phase will need " +
				"(e.g. 'run tests', 'run commands', 'edit files'). " +
				"This helps the user pre-authorize the implementation scope.",
			Items: &tools.Property{
				Type:        "string",
				Description: "A tool category the implementation phase will need.",
			},
		},
	}).JSON()
}

func (t *proposePlanTool) Execute(_ context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	r := t.ref.R
	if r == nil {
		return tools.ToolResult{}, fmt.Errorf("propose_plan: runner not wired")
	}

	// Guard: must be in a planning state (not proposing — already awaiting approval).
	if r.PlanState == PlanStatusProposing {
		return tools.ToolResult{}, fmt.Errorf(
			"propose_plan: a plan is already awaiting approval. Wait for the user's verdict.")
	}
	if r.PlanState != PlanStatusPlanning {
		return tools.ToolResult{}, fmt.Errorf(
			"propose_plan: call enter_plan_mode first to enter the planning phase.")
	}

	planContent := r.lastThinking
	if planContent == "" {
		return tools.ToolResult{}, fmt.Errorf(
			"propose_plan: no plan content found. Write your full plan as your text answer " +
				"BEFORE calling propose_plan, so it can be extracted and reviewed.")
	}

	var in struct {
		AllowedPrompts []string `json:"allowed_prompts"`
	}
	_ = json.Unmarshal(input, &in)

	planID := newPlanID()
	title := r.planTitle
	if title == "" {
		title = "Implementation Plan"
	}

	plan := Plan{
		ID:        planID,
		Title:     title,
		Content:   planContent,
		Status:    PlanStatusProposing,
		CreatedAt: time.Now(),
	}

	// Determine plans directory
	plansDir := t.plansDir
	if plansDir == "" {
		plansDir = filepath.Join(ec.WorkspaceRoot, ".codeagent", "plans")
	}
	if err := os.MkdirAll(plansDir, 0755); err != nil {
		return tools.ToolResult{}, fmt.Errorf("propose_plan: cannot create plans directory: %w", err)
	}

	// Write plan to .codeagent/plans/<id>.md
	planFileName := planID + ".md"
	plan.FilePath = filepath.Join(plansDir, planFileName)
	header := fmt.Sprintf("# %s\n\n_Plan ID: %s | Created: %s_\n\n",
		title, planID, plan.CreatedAt.Format(time.RFC3339))
	if err := os.WriteFile(plan.FilePath, []byte(header+planContent), 0644); err != nil {
		return tools.ToolResult{}, fmt.Errorf("propose_plan: cannot write plan file: %w", err)
	}

	r.PlanState = PlanStatusProposing
	r.activePlan = &plan

	r.emit(Event{
		Kind: EventPlanProposed,
		Text: planContent,
	})

	// Block on user approval — same blocking pattern as Approver.Approve.
	decision := PlanApproved // default when no approver is configured (test/headless path)
	if r.PlanApprover != nil {
		decision = r.PlanApprover.ApprovePlan(plan)
	}

	switch decision {
	case PlanApproved:
		r.PlanState = PlanStatusExecuting
		plan.Status = PlanStatusApproved
		r.emit(Event{Kind: EventPlanApproved, Text: plan.ID})
		promptNote := ""
		if len(in.AllowedPrompts) > 0 {
			promptNote = fmt.Sprintf(" The user has pre-authorized: %v.", in.AllowedPrompts)
		}
		return tools.ToolResult{
			Content: fmt.Sprintf(
				"Plan approved! The plan has been saved to %s. "+
					"You may now implement the plan with full tool access.%s "+
					"Track your progress with todo_write.",
				plan.FilePath, promptNote),
		}, nil
	default:
		r.PlanState = PlanStatusPlanning
		plan.Status = PlanStatusRejected
		r.emit(Event{Kind: EventPlanRejected, Text: plan.ID})
		return tools.ToolResult{
			Content: "Plan rejected. Please revise the plan and call propose_plan again " +
				"when ready.",
		}, nil
	}
}

// NewEnterPlanModeTool creates the enter_plan_mode tool. ref.R is set after
// Runner construction — the tool dereferences it lazily at Execute time.
func NewEnterPlanModeTool(ref *RunnerRef) *enterPlanModeTool {
	return &enterPlanModeTool{ref: ref}
}

// NewProposePlanTool creates the propose_plan tool. ref.R is set after Runner
// construction. plansDir is the absolute path to the plans directory; if empty,
// the tool falls back to <workspace>/.codeagent/plans at execution time.
func NewProposePlanTool(ref *RunnerRef, plansDir string) *proposePlanTool {
	return &proposePlanTool{ref: ref, plansDir: plansDir}
}
