package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (c *Client) checkWorkspaceIdentity() error {
	if c.rootInfo == nil {
		return nil
	}
	root, err := filepath.EvalSymlinks(c.root)
	if err != nil || root != c.root {
		return errors.New("workspace root was removed or redirected")
	}
	r, err := os.Stat(c.root)
	if err != nil || !os.SameFile(c.rootInfo, r) {
		return errors.New("workspace root identity changed")
	}
	g, err := os.Stat(c.gitDir)
	if err != nil || !os.SameFile(c.gitInfo, g) {
		return errors.New("workspace git identity changed")
	}
	return nil
}

func (c *Client) verifyRuntimeIdentity(ctx context.Context) error {
	var info struct {
		Server string `json:"server_id"`
		Store  string `json:"store_id"`
	}
	if err := c.request(ctx, "GET", "/v1/runtime/info", nil, &info); err != nil {
		return err
	}
	if c.binding == nil || info.Server != c.binding.verifiedServerID {
		return errors.New("granted Runtime identity changed")
	}
	info.Server, info.Store = "", ""
	if err := c.request(ctx, "GET", "/v1/tasks/capabilities", nil, &info); err != nil {
		return err
	}
	if info.Server != c.binding.verifiedServerID || info.Store != c.binding.verifiedStoreID {
		return errors.New("granted Runtime/Store identity changed")
	}
	return nil
}

// NewReattachedFromManifest is explicit observation/control reattachment, not
// execution recovery. The owner grant and an externally retained baseline are
// required; dirty work is preserved, never adopted as a new clean baseline.
func NewReattachedFromManifest(ctx context.Context, path, workspace, id, baseline string) (*Client, ConnectionInfo, error) {
	if !validID(id) || !regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`).MatchString(baseline) {
		return nil, ConnectionInfo{}, errors.New("reattach requires an explicit session ID and full baseline commit")
	}
	c, info, err := newFromManifest(ctx, path, workspace, baseline, "")
	if err != nil {
		return nil, info, err
	}
	if info.StoreID == "" {
		c.Close()
		return nil, info, errors.New("reattach requires a verified store identity")
	}
	if err = c.reattach(ctx, id); err != nil {
		c.Close()
		return nil, info, fmt.Errorf("session_reattach: %w", err)
	}
	return c, info, nil
}

func (c *Client) reattach(ctx context.Context, id string) error {
	var detail struct {
		ID        string `json:"id"`
		Workspace string `json:"workspace_path"`
		Policy    string `json:"execution_policy"`
		Archived  string `json:"archived_at"`
		Rebind    bool   `json:"needs_rebind"`
		Fork      string `json:"fork_source_session_id"`
	}
	if err := c.request(ctx, "GET", "/v1/conversations/"+id, nil, &detail); err != nil {
		return err
	}
	if detail.ID != id || detail.Workspace != c.root || detail.Policy != "shared_workspace" || detail.Archived != "" || detail.Rebind || detail.Fork != "" {
		return errors.New("existing session identity, workspace, or execution policy does not match")
	}
	events, err := c.events(ctx, id, 0)
	if err != nil {
		return err
	}
	if err = c.recoverAdmissions(id, events); err != nil {
		return err
	}
	if err = c.checkBinding(); err != nil {
		return err
	}
	c.mu.Lock()
	c.id, c.reattached = id, true
	c.mu.Unlock()
	_, err = c.connect(ctx, id)
	return err
}

// Only durable admissions are recovered. Unknown old payloads cannot be retried
// after restart: callers must read the admitted Turn and use a NEW request ID.
// No tool, approval, input, resume, or cancellation is submitted here.
func (c *Client) recoverAdmissions(id string, events []Event) error {
	var previous int64
	turns := map[string]bool{}
	requests := map[string]bool{}
	for _, e := range events {
		if e.Seq <= previous {
			return errors.New("existing history is unordered or lacks durable sequence identities")
		}
		previous = e.Seq
		if e.Kind != "turn_accepted" {
			continue
		}
		if !validID(e.TurnID) || !validID(e.RequestID) || turns[e.TurnID] || requests[e.RequestID] {
			return errors.New("existing history contains ambiguous Turn/request admissions")
		}
		turns[e.TurnID], requests[e.RequestID] = true, true
	}
	for _, e := range events {
		if e.TurnID != "" && !turns[e.TurnID] {
			return errors.New("existing history lacks an exact Turn admission; reattachment refused")
		}
	}
	for _, e := range events {
		if e.Kind == "turn_accepted" && c.submissions[e.RequestID] == nil {
			c.submissions[e.RequestID] = &submission{receipt: &Receipt{true, id, e.TurnID, e.RequestID, e.Seq}, recovered: true}
		}
	}
	return nil
}

func (c *Client) checkReattachedAdmission(ctx context.Context, requestID string) error {
	if err := c.verifyRuntimeIdentity(ctx); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(c.root)
	if err != nil || root != c.root {
		return errors.New("bound workspace no longer resolves to the same root")
	}
	head, cut, err := gitOutput(ctx, c.root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if cut || strings.TrimSpace(head) != c.baseline {
		return errors.New("workspace baseline changed; refusing new work")
	}
	events, err := c.events(ctx, c.id, 0)
	if err != nil {
		return err
	}
	if err = c.recoverAdmissions(c.id, events); err != nil {
		return err
	}
	if c.submissions[requestID] != nil {
		return errors.New("request_id already admitted before reattachment; read its Turn, do not resubmit")
	}
	var activity struct {
		Delta    *bool `json:"is_delta"`
		Sessions *[]struct {
			ID        string `json:"session_id"`
			State     string `json:"state"`
			Approvals *int   `json:"pending_approval_count"`
			Tools     *int   `json:"pending_client_tool_count"`
		} `json:"sessions"`
	}
	if err = c.request(ctx, "GET", "/v1/activity", nil, &activity); err != nil {
		return err
	}
	if activity.Delta == nil || *activity.Delta || activity.Sessions == nil {
		return errors.New("complete activity snapshot required before new work")
	}
	matches := 0
	for _, row := range *activity.Sessions {
		if row.ID != c.id {
			continue
		}
		matches++
		if (row.State != "idle" && row.State != "done" && row.State != "failed" && row.State != "cancelled") || row.Approvals == nil || *row.Approvals != 0 || row.Tools == nil || *row.Tools != 0 {
			return errors.New("existing session has active work or pending human controls; observe it without resubmission")
		}
	}
	if matches > 1 {
		return errors.New("ambiguous activity snapshot")
	}
	return nil
}
