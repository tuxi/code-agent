package automation

import (
	"context"
	"fmt"
	"strings"
)

// Perm carries the per-task permission context that must be applied to a firing's
// turn. It is the execution-time counterpart of the automation's persisted
// permission_mode / connectors / skills fields (WorkBuddy's per-task "Full
// access" / "Connectors without confirmation").
type Perm struct {
	// PermissionMode is the sandbox/approval tier for this firing. "full_access"
	// auto-approves every side-effecting tool call; "" means the session default.
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

// TurnDispatcher is the default Dispatcher used by the daemon scheduler. It maps
// an Automation onto either a new standalone conversation or an existing chat
// session, then submits the prompt as a turn.
type TurnDispatcher struct {
	Submitter TurnSubmitter
	Creator   ConversationCreator
}

// Dispatch fires the automation once. For standalone mode it creates a new
// conversation (in cwds[0], else CreatedFromWorkspace) and submits there; for
// chat mode it submits into the fixed SessionID. Returns the conversation id the
// firing ran in.
func (d *TurnDispatcher) Dispatch(ctx context.Context, a Automation) (string, error) {
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

// NewTurnDispatcher builds a Dispatcher from the narrow seams. The daemon wiring
// provides conversation-backed implementations of TurnSubmitter and
// ConversationCreator.
func NewTurnDispatcher(submitter TurnSubmitter, creator ConversationCreator) *TurnDispatcher {
	return &TurnDispatcher{Submitter: submitter, Creator: creator}
}

var _ Dispatcher = (*TurnDispatcher)(nil)
