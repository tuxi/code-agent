// Package automation implements the model-facing automation tools: `automation`
// (CRUD over scheduled automations) and `get_current_time` (timezone-aware clock
// primitive). They are registered into the base registry so every conversation
// can create/list/update/delete automations in natural language.
package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"code-agent/internal/automation"
	"code-agent/internal/tools"
)

// AutomationTool is the single-tool, multi-mode CRUD entry point (R12), mirroring
// workbuddy's automation_update. mode selects list|view|create|update|delete.
type AutomationTool struct{}

func (*AutomationTool) Name() string { return "automation" }

func (*AutomationTool) Description() string {
	return "Create, list, view, update, or delete scheduled automations. " +
		"An automation runs a prompt on a schedule (once or recurring) in the background. " +
		"mode is required: list (summaries), view (full config), create, update (only provided fields change), delete (soft). " +
		"create requires name, prompt, schedule_type (once|recurring), and timezone; once needs scheduled_at, recurring needs rrule. " +
		"Call get_current_time first to confirm the timezone before creating."
}

func (*AutomationTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"mode": {"type": "string", "enum": ["list", "view", "create", "update", "delete"]},
			"id": {"type": "string", "description": "automation id; required for view/update/delete"},
			"name": {"type": "string"},
			"prompt": {"type": "string", "description": "the instruction run on each firing"},
			"schedule_type": {"type": "string", "enum": ["once", "recurring"]},
			"rrule": {"type": "string", "description": "recurring rule, e.g. FREQ=DAILY;BYHOUR=16;BYMINUTE=0"},
			"scheduled_at": {"type": "string", "description": "once: ISO8601 firing time"},
			"timezone": {"type": "string", "description": "IANA timezone, e.g. America/Los_Angeles"},
			"mode_exec": {"type": "string", "enum": ["standalone", "chat", "reuse"], "description": "standalone=new conversation each firing; chat=return to session_id; reuse=reuse the first firing's conversation (default for recurring)"},
			"session_id": {"type": "string", "description": "chat mode: the session to return to"},
			"cwds": {"type": "array", "items": {"type": "string"}, "description": "optional target workspaces"},
			"model_id": {"type": "string"},
			"skills": {"type": "array", "items": {"type": "string"}},
			"connectors": {"type": "array", "items": {"type": "string"}},
			"permission_mode": {"type": "string"},
			"enabled": {"type": "boolean", "description": "create/update: ACTIVE (true) or PAUSED (false)"}
		},
		"additionalProperties": false
	}`)
}

type automationInput struct {
	Mode           string   `json:"mode"`
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Prompt         string   `json:"prompt"`
	ScheduleType   string   `json:"schedule_type"`
	RRule          string   `json:"rrule"`
	ScheduledAt    string   `json:"scheduled_at"`
	Timezone       string   `json:"timezone"`
	ModeExec       string   `json:"mode_exec"`
	SessionID      string   `json:"session_id"`
	CWDs           []string `json:"cwds"`
	ModelID        string   `json:"model_id"`
	Skills         []string `json:"skills"`
	Connectors     []string `json:"connectors"`
	PermissionMode string   `json:"permission_mode"`
	Enabled        *bool    `json:"enabled"`
}

func (*AutomationTool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in automationInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("automation: parse input: %w", err)
	}
	if ec.AutomationStore == nil {
		return tools.ToolResult{}, fmt.Errorf("automation: automation store is not available in this host")
	}
	store := ec.AutomationStore

	switch in.Mode {
	case "list":
		items, err := store.List(ctx)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("automation: list: %w", err)
		}
		out, _ := json.Marshal(items)
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "view":
		if in.ID == "" {
			return tools.ToolResult{}, fmt.Errorf("automation: view requires id")
		}
		a, err := store.Get(ctx, in.ID)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("automation: view: %w", err)
		}
		out, _ := json.Marshal(a)
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "create":
		a, err := buildAutomation(in, ec)
		if err != nil {
			return tools.ToolResult{}, err
		}
		created, err := store.Create(ctx, a)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("automation: create: %w", err)
		}
		out, _ := json.Marshal(created)
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "update":
		if in.ID == "" {
			return tools.ToolResult{}, fmt.Errorf("automation: update requires id")
		}
		patch, err := buildPatch(in)
		if err != nil {
			return tools.ToolResult{}, err
		}
		updated, err := store.Update(ctx, in.ID, patch)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("automation: update: %w", err)
		}
		out, _ := json.Marshal(updated)
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "delete":
		if in.ID == "" {
			return tools.ToolResult{}, fmt.Errorf("automation: delete requires id")
		}
		if err := store.Delete(ctx, in.ID); err != nil {
			return tools.ToolResult{}, fmt.Errorf("automation: delete: %w", err)
		}
		return tools.ToolResult{Content: "deleted " + in.ID}, nil

	default:
		return tools.ToolResult{}, fmt.Errorf("automation: unknown mode %q (list|view|create|update|delete)", in.Mode)
	}
}

// buildAutomation validates create inputs and maps them onto an Automation.
func buildAutomation(in automationInput, ec tools.ExecutionContext) (automation.Automation, error) {
	if in.Name == "" {
		return automation.Automation{}, fmt.Errorf("automation: create requires name")
	}
	if in.Prompt == "" {
		return automation.Automation{}, fmt.Errorf("automation: create requires prompt")
	}
	if in.Timezone == "" {
		return automation.Automation{}, fmt.Errorf("automation: create requires timezone (call get_current_time first)")
	}
	st := automation.ScheduleRecurring
	if in.ScheduleType == "once" {
		st = automation.ScheduleOnce
	} else if in.ScheduleType != "" && in.ScheduleType != "recurring" {
		return automation.Automation{}, fmt.Errorf("automation: schedule_type must be once or recurring")
	}
	if st == automation.ScheduleOnce && in.ScheduledAt == "" {
		return automation.Automation{}, fmt.Errorf("automation: once requires scheduled_at")
	}
	if st == automation.ScheduleRecurring && in.RRule == "" {
		return automation.Automation{}, fmt.Errorf("automation: recurring requires rrule")
	}
	mode := automation.ModeStandalone
	if in.ModeExec == "chat" {
		mode = automation.ModeChat
	} else if in.ModeExec == "reuse" {
		mode = automation.ModeReuse
	} else if in.ModeExec != "" && in.ModeExec != "standalone" {
		return automation.Automation{}, fmt.Errorf("automation: mode_exec must be standalone, chat, or reuse")
	}
	// Default: a recurring task reuses one conversation (no pile-up of one
	// conversation per firing, and LLM context caching applies); a once task is
	// standalone. The user can override in the client.
	if in.ModeExec == "" && st == automation.ScheduleRecurring {
		mode = automation.ModeReuse
	}
	if mode == automation.ModeChat && in.SessionID == "" {
		return automation.Automation{}, fmt.Errorf("automation: chat mode requires session_id")
	}
	status := automation.StatusActive
	if in.Enabled != nil && !*in.Enabled {
		status = automation.StatusPaused
	}
	// Default permission tier: full. An automation runs unattended — if it
	// blocks on a human approval nobody is watching, the automation is useless
	// (e.g. a nightly BTC check that stalls on an approval prompt). Since the
	// user explicitly created an automation, default to letting it act; the
	// tier goes through the same ModeApprover as interactive sessions, so deny
	// rules and protected paths still hold. The skill and the client form
	// surface this so the user knows the task's permission level, and can
	// narrow it (auto / ask, or inherit the workspace tier with "") for
	// high-risk tasks. Values are canonicalized via NormalizePermissionMode
	// ("full_access" is accepted as the legacy alias of "full").
	permissionMode, ok := automation.NormalizePermissionMode(in.PermissionMode)
	if !ok {
		return automation.Automation{}, fmt.Errorf("automation: permission_mode must be one of ask, auto, full")
	}
	if permissionMode == "" {
		permissionMode = "full"
	}
	var scheduledAt time.Time
	if in.ScheduledAt != "" {
		t, err := time.Parse(time.RFC3339, in.ScheduledAt)
		if err != nil {
			return automation.Automation{}, fmt.Errorf("automation: scheduled_at must be RFC3339: %w", err)
		}
		scheduledAt = t
	}
	// If the user did not specify a model, use the creating session's model
	// (Problem 1: fixtures in a conversation should run with that conversation's
	// model, never an arbitrary default from settings.json whose provider may be
	// out of quota). ec.Model carries the parent turn's resolved model name.
	modelID := in.ModelID
	if modelID == "" {
		modelID = ec.Model
	}
	// If the user did not specify target workspaces, default to the creating
	// session's workspace so the client can always show where the task runs
	// (cwds is the display field; createdFromWorkspace is the fallback source).
	cwds := in.CWDs
	if len(cwds) == 0 && ec.WorkspaceRoot != "" {
		cwds = []string{ec.WorkspaceRoot}
	}
	return automation.Automation{
		Name:                 in.Name,
		Prompt:               in.Prompt,
		Status:               status,
		ScheduleType:         st,
		RRule:                in.RRule,
		ScheduledAt:          scheduledAt,
		Timezone:             in.Timezone,
		ModeExec:             mode,
		SessionID:            in.SessionID,
		CWDs:                 cwds,
		ModelID:              modelID,
		Skills:               in.Skills,
		Connectors:           in.Connectors,
		PermissionMode:       permissionMode,
		CreatedFromWorkspace: ec.WorkspaceRoot,
	}, nil
}

// buildPatch maps update inputs onto a partial patch (only provided fields).
func buildPatch(in automationInput) (automation.AutomationPatch, error) {
	var p automation.AutomationPatch
	if in.Name != "" {
		p.Name = &in.Name
	}
	if in.Prompt != "" {
		p.Prompt = &in.Prompt
	}
	if in.ScheduleType != "" {
		st := automation.ScheduleType(in.ScheduleType)
		p.ScheduleType = &st
	}
	if in.RRule != "" {
		p.RRule = &in.RRule
	}
	if in.ScheduledAt != "" {
		t, err := time.Parse(time.RFC3339, in.ScheduledAt)
		if err != nil {
			return p, fmt.Errorf("automation: scheduled_at must be RFC3339: %w", err)
		}
		p.ScheduledAt = &t
	}
	if in.Timezone != "" {
		p.Timezone = &in.Timezone
	}
	if in.ModeExec != "" {
		m := automation.RunMode(in.ModeExec)
		p.ModeExec = &m
	}
	if in.SessionID != "" {
		p.SessionID = &in.SessionID
	}
	if in.CWDs != nil {
		p.CWDs = &in.CWDs
	}
	if in.ModelID != "" {
		p.ModelID = &in.ModelID
	}
	if in.Skills != nil {
		p.Skills = &in.Skills
	}
	if in.Connectors != nil {
		p.Connectors = &in.Connectors
	}
	if in.PermissionMode != "" {
		m, ok := automation.NormalizePermissionMode(in.PermissionMode)
		if !ok {
			return p, fmt.Errorf("automation: permission_mode must be one of ask, auto, full")
		}
		p.PermissionMode = &m
	}
	if in.Enabled != nil {
		st := automation.StatusActive
		if !*in.Enabled {
			st = automation.StatusPaused
		}
		p.Status = &st
	}
	return p, nil
}

var _ tools.Tool = (*AutomationTool)(nil)
