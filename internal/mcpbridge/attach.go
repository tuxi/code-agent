package mcpbridge

import (
	"context"
	"errors"
)

// AttachEmptySession explicitly binds one existing, empty session after a bridge
// restart. This is not turn recovery: it never creates a conversation, opens a
// socket, approves an operation, or submits/resumes execution. New still requires
// a clean workspace. Inspection is read-only, not an atomic execution lease;
// the pilot must keep other clients from submitting to this session meanwhile.
func (c *Client) AttachEmptySession(ctx context.Context, id string) (SessionRef, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.requireWritable(); err != nil {
		return SessionRef{}, err
	}
	if !validID(id) {
		return SessionRef{}, errors.New("valid session_id is required")
	}
	if c.binding == nil || c.binding.verifiedStoreID == "" {
		return SessionRef{}, errors.New("empty-session binding requires a manifest with verified runtime and store identities")
	}
	c.mu.Lock()
	bound := c.id != ""
	c.mu.Unlock()
	if bound {
		return SessionRef{}, errors.New("this bridge is already bound to a session")
	}
	var detail struct {
		ID              string `json:"id"`
		WorkspacePath   string `json:"workspace_path"`
		TurnCount       *int   `json:"turn_count"`
		MessageCount    *int   `json:"message_count"`
		TurnStatus      string `json:"turn_status"`
		ExecutionPolicy string `json:"execution_policy"`
		ArchivedAt      string `json:"archived_at"`
		NeedsRebind     bool   `json:"needs_rebind"`
		ForkSource      string `json:"fork_source_session_id"`
	}
	if err := c.request(ctx, "GET", "/v1/conversations/"+id, nil, &detail); err != nil {
		return SessionRef{}, err
	}
	if detail.ID != id || detail.WorkspacePath != c.root || detail.TurnCount == nil || *detail.TurnCount != 0 || detail.MessageCount == nil || *detail.MessageCount != 0 || (detail.TurnStatus != "" && detail.TurnStatus != "idle") || detail.ExecutionPolicy != "shared_workspace" || detail.ArchivedAt != "" || detail.NeedsRebind || detail.ForkSource != "" {
		return SessionRef{}, errors.New("session is not an empty, idle, unarchived session in the bound workspace")
	}
	// Durable history and live scheduler/broker state are checked independently;
	// a zero-turn cached header alone cannot establish that a session is unused.
	events, err := c.events(ctx, id, 0)
	if err != nil {
		return SessionRef{}, err
	}
	if len(events) != 0 {
		return SessionRef{}, errors.New("empty-session binding refuses any existing durable event history")
	}
	type activityRow struct {
		ID          string `json:"session_id"`
		State       string `json:"state"`
		TurnID      string `json:"turn_id"`
		ActiveTurn  string `json:"active_turn_id"`
		Queue       *int   `json:"queue_position"`
		Approvals   *int   `json:"pending_approval_count"`
		ClientTools *int   `json:"pending_client_tool_count"`
		LastSeq     *int64 `json:"last_sequence"`
		ArchivedAt  string `json:"archived_at"`
		Terminal    any    `json:"latest_terminal"`
	}
	var activity struct {
		IsDelta  *bool          `json:"is_delta"`
		Sessions *[]activityRow `json:"sessions"`
	}
	if err := c.request(ctx, "GET", "/v1/activity", nil, &activity); err != nil {
		return SessionRef{}, err
	}
	if activity.IsDelta == nil || *activity.IsDelta || activity.Sessions == nil {
		return SessionRef{}, errors.New("runtime did not provide a complete activity snapshot")
	}
	matches := 0
	for _, a := range *activity.Sessions {
		if a.ID != id {
			continue
		}
		matches++
		if a.State != "idle" || a.TurnID != "" || a.ActiveTurn != "" || a.Queue == nil || *a.Queue != 0 || a.Approvals == nil || *a.Approvals != 0 || a.ClientTools == nil || *a.ClientTools != 0 || a.LastSeq == nil || *a.LastSeq != 0 || a.ArchivedAt != "" || a.Terminal != nil {
			return SessionRef{}, errors.New("session has live work, pending controls, or previous execution")
		}
	}
	// /v1/activity is a sparse projection: a completely unused idle session can
	// be absent. A full snapshot must still contain the sessions array and must
	// not report duplicate or non-idle entries for this session.
	if matches > 1 {
		return SessionRef{}, errors.New("runtime reported duplicate session activity records")
	}
	if err := c.checkBinding(); err != nil {
		return SessionRef{}, err
	}
	c.mu.Lock()
	c.id = id
	c.mu.Unlock()
	return SessionRef{ID: id, WorkspacePath: c.root}, nil
}
