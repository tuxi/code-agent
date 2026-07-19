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
	"code-agent/internal/workspace"
)

const maxPlanFileBytes int64 = 200_000

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
			"concrete implementation plan. Write the complete plan to a markdown file " +
			"under .codeagent/plans/. When it is complete, call propose_plan with " +
			"plan_path set to that workspace-relative file path.",
	}, nil
}

// proposePlanTool presents the model's plan to the user for approval. The
// canonical form is a markdown file written under .codeagent/plans/ and named
// explicitly by plan_path. lastAssistantText remains as a compatibility fallback for
// older callers that submit the plan inline before invoking this tool.
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
		"Write the full plan to a markdown file under .codeagent/plans/, then pass " +
		"that workspace-relative file path as plan_path. Keep any accompanying text brief. " +
		"The user will review the plan and approve or reject it. " +
		"If approved you may proceed to implement with full tool access. " +
		"If rejected you must revise the plan."
}

func (t *proposePlanTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"plan_path": {
			Type: "string",
			Description: "Workspace-relative path to the complete markdown plan under " +
				".codeagent/plans/ (for example .codeagent/plans/add-auth.md).",
		},
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

	var in struct {
		PlanPath       string   `json:"plan_path"`
		AllowedPrompts []string `json:"allowed_prompts"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("propose_plan: invalid input: %w", err)
		}
	}

	planID := newPlanID()
	title := r.planTitle
	if title == "" {
		title = "Implementation Plan"
	}

	// Determine plans directory
	plansDir := t.plansDir
	if plansDir == "" {
		plansDir = filepath.Join(ec.WorkspaceRoot, ".codeagent", "plans")
	}
	if err := os.MkdirAll(plansDir, 0755); err != nil {
		return tools.ToolResult{}, fmt.Errorf("propose_plan: cannot create plans directory: %w", err)
	}

	plan := Plan{
		ID:        planID,
		Title:     title,
		Status:    PlanStatusProposing,
		CreatedAt: time.Now(),
	}

	if in.PlanPath != "" {
		planContent, filePath, relativePath, err := loadPlanFile(ec.WorkspaceRoot, plansDir, in.PlanPath)
		if err != nil {
			return tools.ToolResult{}, err
		}
		plan.Content = planContent
		plan.FilePath = filePath
		plan.WorkspaceRelativePath = relativePath
	} else {
		// Compatibility path for older models: preserve the previous inline-plan
		// behavior until all callers send plan_path.
		if r.lastAssistantText == "" {
			return tools.ToolResult{}, fmt.Errorf(
				"propose_plan: plan_path is required. Write the complete plan under " +
					".codeagent/plans/ and pass its workspace-relative path")
		}
		plan.Content = r.lastAssistantText
		plan.FilePath = filepath.Join(plansDir, planID+".md")
		header := fmt.Sprintf("# %s\n\n_Plan ID: %s | Created: %s_\n\n",
			title, planID, plan.CreatedAt.Format(time.RFC3339))
		if err := os.WriteFile(plan.FilePath, []byte(header+plan.Content), 0644); err != nil {
			return tools.ToolResult{}, fmt.Errorf("propose_plan: cannot write legacy plan file: %w", err)
		}
		plan.WorkspaceRelativePath = relativePlanPath(ec.WorkspaceRoot, plan.FilePath)
	}

	r.PlanState = PlanStatusProposing
	r.activePlan = &plan

	r.emit(Event{
		Kind: EventPlanProposed,
		Text: plan.Content,
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

func loadPlanFile(workspaceRoot, plansDir, requestedPath string) (content, filePath, relativePath string, err error) {
	if filepath.IsAbs(requestedPath) {
		return "", "", "", fmt.Errorf("propose_plan: plan_path must be workspace-relative")
	}
	if filepath.Ext(requestedPath) != ".md" {
		return "", "", "", fmt.Errorf("propose_plan: plan_path must point to a .md file")
	}

	target := filepath.Join(workspaceRoot, filepath.Clean(requestedPath))
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", "", fmt.Errorf("propose_plan: resolve plan_path: %w", err)
	}
	canonicalPlansDir, err := workspace.CanonicalPath(plansDir)
	if err != nil {
		return "", "", "", fmt.Errorf("propose_plan: resolve plans directory: %w", err)
	}
	canonicalTarget, err := workspace.CanonicalPath(target)
	if err != nil {
		return "", "", "", fmt.Errorf("propose_plan: resolve plan_path: %w", err)
	}
	if !workspace.IsSubPath(canonicalPlansDir, canonicalTarget) || canonicalTarget == canonicalPlansDir {
		return "", "", "", fmt.Errorf("propose_plan: plan_path must be inside .codeagent/plans/")
	}

	info, err := os.Stat(canonicalTarget)
	if err != nil {
		return "", "", "", fmt.Errorf("propose_plan: read plan_path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", "", fmt.Errorf("propose_plan: plan_path must be a regular file")
	}
	if info.Size() > maxPlanFileBytes {
		return "", "", "", fmt.Errorf("propose_plan: plan file is too large: %d bytes (max %d)", info.Size(), maxPlanFileBytes)
	}
	raw, err := os.ReadFile(canonicalTarget)
	if err != nil {
		return "", "", "", fmt.Errorf("propose_plan: read plan_path: %w", err)
	}
	if len(raw) == 0 {
		return "", "", "", fmt.Errorf("propose_plan: plan file is empty")
	}
	return string(raw), canonicalTarget, relativePlanPath(workspaceRoot, canonicalTarget), nil
}

func relativePlanPath(workspaceRoot, filePath string) string {
	canonicalRoot, err := workspace.CanonicalPath(workspaceRoot)
	if err != nil {
		return ""
	}
	canonicalFile, err := workspace.CanonicalPath(filePath)
	if err != nil || !workspace.IsSubPath(canonicalRoot, canonicalFile) {
		return ""
	}
	rel, err := filepath.Rel(canonicalRoot, canonicalFile)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
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
