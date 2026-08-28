package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Perm carries the per-task permission context that must be applied to a firing's
// turn. It is the execution-time counterpart of the automation's persisted
// permission_mode / connectors / skills fields (WorkBuddy's per-task "Full
// access" / "Connectors without confirmation").
type Perm struct {
	// PermissionMode is the approval tier for this firing: "ask", "auto", or
	// "full" (canonical values, see NormalizePermissionMode). "full" runs every
	// side-effecting call through the tier policy with hard lines intact (deny
	// rules, protected paths, blocked commands); "" means the workspace default
	// tier applies.
	PermissionMode string
	// Connectors are MCP server names authorized for use WITHOUT confirmation
	// during this firing. Tool calls to these servers are auto-approved.
	Connectors []string
	// Skills are skill names to auto-load at firing (reserved; the prompt already
	// carries the instruction, skills are loaded by the model as needed).
	Skills []string
}

// TurnSubmitter submits a headless agent turn into a conversation. It is the
// narrow seam the dispatcher needs — decoupled from the full TurnExecutor so the
// scheduler can be tested against a fake. The daemon wiring provides a
// conversation-backed implementation.
type TurnSubmitter interface {
	// Submit runs a turn in sessionID with the given prompt as a headless turn and
	// returns the turn id of the dispatched conversation. model is the optional
	// model profile name; empty means the server default. perm carries the
	// per-task permission context to apply to this firing (zero value = session
	// default approval).
	Submit(ctx context.Context, sessionID string, prompt string, model string, perm Perm) (turnID string, err error)
}

// ConversationCreator creates a new standalone conversation. The daemon wiring
// provides a conversation-repository-backed implementation.
type ConversationCreator interface {
	// CreateConversation makes a new session in workspacePath and returns its id.
	CreateConversation(ctx context.Context, workspacePath string) (sessionID string, err error)
	// DeleteConversation removes a session the dispatcher created but could not use
	// (e.g. the subsequent submit failed), so a failed firing does not leak an
	// empty orphan conversation. Optional: a nil implementation skips cleanup.
	DeleteConversation(ctx context.Context, sessionID string) error
	// ConversationExists reports whether a conversation still exists. Reuse mode
	// uses it to detect a deleted session and fall back to creating a new one.
	ConversationExists(ctx context.Context, sessionID string) (bool, error)
}

// WorkflowRunner is the counterpart of TurnSubmitter for the workflow execution
// mode. When an automation's WorkflowRef is set, the dispatcher calls this
// instead of the prompt turn path — zero LLM cost, deterministic execution.
type WorkflowRunner interface {
	// SubmitWorkflowRun triggers a saved workflow template by name. Returns the
	// task id of the asynchronous workflow run.
	SubmitWorkflowRun(ctx context.Context, workspaceRoot, workflowName string, input map[string]any) (int64, error)
	// HasActiveWorkflowRun reports whether the workflow has a non-terminal run
	// (pending/running/suspended) — the overlap-policy basis.
	HasActiveWorkflowRun(ctx context.Context, workspaceRoot, workflowName string) (bool, error)
}

// TurnDispatcher is the default Dispatcher used by the daemon scheduler. It maps
// an Automation onto either a new standalone conversation or an existing chat
// session, then submits the prompt as a turn. When WorkflowRef is set, it
// dispatches a workflow run instead — zero LLM cost, deterministic execution.
type TurnDispatcher struct {
	Submitter      TurnSubmitter
	Creator        ConversationCreator
	WorkflowRunner WorkflowRunner
}

// Dispatch fires the automation once. For standalone mode it creates a new
// conversation (in cwds[0], else CreatedFromWorkspace) and submits there; for
// chat mode it submits into the fixed SessionID. When WorkflowRef is set, it
// triggers a workflow template directly instead of a prompt turn (zero LLM
// cost, deterministic execution). Returns the conversation id or task id the
// firing ran in.
func (d *TurnDispatcher) Dispatch(ctx context.Context, a Automation) (string, error) {
	// Workflow execution mode: trigger a template directly, zero LLM.
	if a.WorkflowRef != "" {
		return d.dispatchWorkflow(ctx, a)
	}
	if d.Submitter == nil {
		return "", fmt.Errorf("automation: dispatcher has no submitter")
	}
	perm := Perm{
		PermissionMode: a.PermissionMode,
		Connectors:     a.Connectors,
		Skills:         a.Skills,
	}
	switch a.ModeExec {
	case ModeChat:
		if strings.TrimSpace(a.SessionID) == "" {
			return "", fmt.Errorf("automation %q: chat mode requires session_id", a.ID)
		}
		return d.Submitter.Submit(ctx, a.SessionID, a.Prompt, a.ModelID, perm)
	case ModeReuse:
		// Reuse the first firing's conversation: once a session_id is persisted,
		// every later firing returns to it (context caching, no conversation pile-up).
		// If the persisted conversation no longer exists (the user deleted it),
		// fall through and create a fresh one.
		if strings.TrimSpace(a.SessionID) != "" && d.Creator != nil {
			if exists, err := d.Creator.ConversationExists(ctx, a.SessionID); err == nil && exists {
				return d.Submitter.Submit(ctx, a.SessionID, a.Prompt, a.ModelID, perm)
			}
		}
		// First firing, or the persisted conversation was deleted: create a new one
		// (same workspace resolution as standalone) and submit there. The scheduler
		// persists the returned id as the automation's session_id so later firings
		// reuse it.
		ws := ""
		if len(a.CWDs) > 0 {
			ws = a.CWDs[0]
		} else if a.CreatedFromWorkspace != "" {
			ws = a.CreatedFromWorkspace
		}
		if d.Creator == nil {
			return "", fmt.Errorf("automation %q: reuse mode requires a conversation creator", a.ID)
		}
		sid, err := d.Creator.CreateConversation(ctx, ws)
		if err != nil {
			return "", fmt.Errorf("automation %q: create reuse conversation: %w", a.ID, err)
		}
		if _, err := d.Submitter.Submit(ctx, sid, a.Prompt, a.ModelID, perm); err != nil {
			return "", fmt.Errorf("automation %q: submit reuse turn: %w", a.ID, err)
		}
		return sid, nil
	case ModeStandalone:
		ws := ""
		if len(a.CWDs) > 0 {
			ws = a.CWDs[0]
		} else if a.CreatedFromWorkspace != "" {
			ws = a.CreatedFromWorkspace
		}
		if d.Creator == nil {
			return "", fmt.Errorf("automation %q: standalone mode requires a conversation creator", a.ID)
		}
		sid, err := d.Creator.CreateConversation(ctx, ws)
		if err != nil {
			return "", fmt.Errorf("automation %q: create standalone conversation: %w", a.ID, err)
		}
		if _, err := d.Submitter.Submit(ctx, sid, a.Prompt, a.ModelID, perm); err != nil {
			// The conversation was created but the turn could not be submitted
			// (e.g. the model ran out of quota). Keep the conversation so the user
			// can open it and see the failure (the submitter records the error into
			// the session); the retry cap (MaxRetries) bounds how many such
			// conversations a failing automation can create.
			return "", fmt.Errorf("automation %q: submit standalone turn: %w", a.ID, err)
		}
		return sid, nil
	default:
		return "", fmt.Errorf("automation %q: unsupported mode_exec %q", a.ID, a.ModeExec)
	}
}

// dispatchWorkflow triggers a workflow template. It enforces the overlap policy,
// then calls SubmitWorkflowRun and returns the task id as the firing identifier.
func (d *TurnDispatcher) dispatchWorkflow(ctx context.Context, a Automation) (string, error) {
	parts := strings.SplitN(a.WorkflowRef, "#", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("automation %q: invalid workflow_ref %q, expected workspace_path#workflow_name", a.ID, a.WorkflowRef)
	}
	ws, name := parts[0], parts[1]

	var input map[string]any
	if a.WorkflowInput != "" {
		if err := json.Unmarshal([]byte(a.WorkflowInput), &input); err != nil {
			return "", fmt.Errorf("automation %q: parse workflow_input: %w", a.ID, err)
		}
	}

	// Overlap policy: skip if a run is already active.
	policy := a.OverlapPolicy
	if policy == "" {
		policy = "skip"
	}
	if policy == "skip" && d.WorkflowRunner != nil {
		active, err := d.WorkflowRunner.HasActiveWorkflowRun(ctx, ws, name)
		if err != nil {
			return "", fmt.Errorf("automation %q: overlap check: %w", a.ID, err)
		}
		if active {
			return "", nil // skip — handled by scheduler as "no error, no turn id"
		}
	}

	taskID, err := d.WorkflowRunner.SubmitWorkflowRun(ctx, ws, name, input)
	if err != nil {
		return "", fmt.Errorf("automation %q: workflow run: %w", a.ID, err)
	}
	return fmt.Sprintf("wf_%d", taskID), nil
}

// taskIDField extracts the workflow task id from a dispatcher return value
// ("wf_<taskID>"), empty for prompt-turn firings (conversation ids).
func taskIDField(dispatchResult string) string {
	if strings.HasPrefix(dispatchResult, "wf_") {
		return strings.TrimPrefix(dispatchResult, "wf_")
	}
	return ""
}

// NewTurnDispatcher builds a Dispatcher from the narrow seams. The daemon wiring
// provides conversation-backed implementations of TurnSubmitter and
// ConversationCreator.
func NewTurnDispatcher(submitter TurnSubmitter, creator ConversationCreator) *TurnDispatcher {
	return &TurnDispatcher{Submitter: submitter, Creator: creator}
}

// SetWorkflowRunner wires the workflow execution mode into the dispatcher. Nil
// (or unset) leaves workflow_ref automations failing with a clear error.
func (d *TurnDispatcher) SetWorkflowRunner(runner WorkflowRunner) {
	d.WorkflowRunner = runner
}

var _ Dispatcher = (*TurnDispatcher)(nil)
