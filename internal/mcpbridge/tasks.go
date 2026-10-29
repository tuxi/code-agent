package mcpbridge

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"code-agent/internal/collab"
)

// taskScope derives workspace identity from the already bound Session. The
// owner bearer is wider than the Bridge grant and must never define its scope.
func (c *Client) taskScope(ctx context.Context) (string, string, error) {
	c.mu.Lock()
	id := c.id
	c.mu.Unlock()
	if c.binding == nil || c.binding.verifiedStoreID == "" || c.binding.verifiedServerID == "" || id == "" {
		return "", "", errors.New("task tools require a verified Runtime/Store grant and an explicitly bound Session")
	}
	if err := c.verifyRuntimeIdentity(ctx); err != nil {
		return "", "", err
	}
	var detail struct {
		ID          string `json:"id"`
		Path        string `json:"workspace_path"`
		Archived    string `json:"archived_at"`
		NeedsRebind bool   `json:"needs_rebind"`
		Policy      string `json:"execution_policy"`
		Workspace   *struct {
			Root string `json:"root"`
			Rel  string `json:"rel"`
			Ext  string `json:"ext_id"`
		} `json:"workspace_ref"`
	}
	if err := c.request(ctx, "GET", "/v1/conversations/"+id, nil, &detail); err != nil {
		return "", "", err
	}
	if detail.ID != id || detail.Path != c.root || detail.Archived != "" || detail.NeedsRebind || detail.Policy != "shared_workspace" {
		return "", "", errors.New("bound Session/workspace is no longer available")
	}
	identity := c.root
	if detail.Workspace != nil && detail.Workspace.Root != "" {
		identity = detail.Workspace.Root + "\x00" + detail.Workspace.Rel + "\x00" + detail.Workspace.Ext
	}
	return id, "workspace_" + collab.PayloadHash([]byte(identity)), nil
}

func (c *Client) taskCapabilities(ctx context.Context, extra ...string) error {
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := c.request(ctx, "GET", "/v1/tasks/capabilities", nil, &out); err != nil {
		return err
	}
	found := map[string]bool{}
	for _, v := range out.Capabilities {
		found[v] = true
	}
	if !found["task_managed_workflow_v1"] || !found["task_review_gate_v1"] {
		return collab.ErrUnavailable
	}
	for _, capability := range extra {
		if !found[capability] {
			return collab.ErrUnavailable
		}
	}
	return nil
}

func (c *Client) ReadTask(ctx context.Context, id string) (collab.Snapshot, error) {
	var out collab.Snapshot
	if !validID(id) {
		return out, collab.ErrInvalid
	}
	sessionID, workspace, err := c.taskScope(ctx)
	if err != nil {
		return out, err
	}
	if err = c.request(ctx, "GET", "/v1/tasks/"+id+"?scope_workspace_ref="+url.QueryEscape(workspace)+"&scope_session_id="+url.QueryEscape(sessionID), nil, &out); err != nil {
		return out, err
	}
	if out.TaskID != id || out.WorkspaceRef != workspace {
		return collab.Snapshot{}, collab.ErrForbidden
	}
	for _, p := range out.Participations {
		if p.SessionRef.Kind == "local" && (p.SessionRef.SessionID != sessionID || p.SessionRef.ServerID != c.binding.verifiedServerID || p.SessionRef.StoreID != c.binding.verifiedStoreID) {
			return collab.Snapshot{}, collab.ErrForbidden
		}
	}
	return out, nil
}

func (c *Client) CreateTask(ctx context.Context, request, goal string, acceptance []string) (collab.Receipt, error) {
	id, _, err := c.taskScope(ctx)
	if err != nil {
		return collab.Receipt{}, err
	}
	var out collab.Receipt
	err = c.request(ctx, "POST", "/v1/tasks", collab.CreateInput{RequestID: request, WorkspaceSessionID: id, Goal: goal, Acceptance: acceptance}, &out)
	if err == nil {
		_, err = c.ReadTask(ctx, out.ID)
	}
	return out, err
}

func (c *Client) RegisterTaskAgent(ctx context.Context, in collab.ExternalInput) (collab.SessionRef, error) {
	if _, _, err := c.taskScope(ctx); err != nil {
		return collab.SessionRef{}, err
	}
	var out collab.SessionRef
	err := c.request(ctx, "POST", "/v1/task-agent-sessions", in, &out)
	return out, err
}

func (c *Client) ParticipateTask(ctx context.Context, id string, in collab.ParticipationInput) (collab.Receipt, error) {
	if _, err := c.ReadTask(ctx, id); err != nil {
		return collab.Receipt{}, err
	}
	c.mu.Lock()
	sessionID := c.id
	c.mu.Unlock()
	if in.SessionRef.Kind == "local" && (in.SessionRef.SessionID != sessionID || in.SessionRef.ServerID != c.binding.verifiedServerID || in.SessionRef.StoreID != c.binding.verifiedStoreID) {
		return collab.Receipt{}, collab.ErrForbidden
	}
	var out collab.Receipt
	err := c.request(ctx, "POST", "/v1/tasks/"+id+"/participations", in, &out)
	return out, err
}

func (c *Client) ReportTask(ctx context.Context, id, kind string, in collab.ReportInput) (collab.Receipt, error) {
	if kind != "plan" && kind != "review" {
		return collab.Receipt{}, collab.ErrInvalid
	}
	if _, err := c.ReadTask(ctx, id); err != nil {
		return collab.Receipt{}, err
	}
	var out collab.Receipt
	err := c.request(ctx, "POST", "/v1/tasks/"+id+"/"+kind+"s", in, &out)
	return out, err
}

func (c *Client) LinkTaskTurn(ctx context.Context, id string, in collab.AttemptInput) (collab.Receipt, error) {
	snap, err := c.ReadTask(ctx, id)
	if err != nil {
		return collab.Receipt{}, err
	}
	if snap.ManagedRun != nil {
		return collab.Receipt{}, collab.ErrManagedWorkspace
	}
	var out collab.Receipt
	err = c.request(ctx, "POST", "/v1/tasks/"+id+"/attempts", in, &out)
	return out, err
}

func (c *Client) LinkTaskArtifact(ctx context.Context, id string, in collab.ArtifactInput) (collab.Receipt, error) {
	snap, err := c.ReadTask(ctx, id)
	if err != nil {
		return collab.Receipt{}, err
	}
	if snap.ManagedRun != nil {
		return collab.Receipt{}, collab.ErrManagedWorkspace
	}
	var out collab.Receipt
	err = c.request(ctx, "POST", "/v1/tasks/"+id+"/artifacts", in, &out)
	return out, err
}

func (c *Client) TaskEvents(ctx context.Context, id, after string, limit int) (collab.EventPage, error) {
	if _, err := c.ReadTask(ctx, id); err != nil {
		return collab.EventPage{}, err
	}
	if after == "" {
		after = "0"
	}
	n, err := strconv.ParseInt(after, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != after {
		return collab.EventPage{}, collab.ErrInvalid
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 200 {
		return collab.EventPage{}, collab.ErrInvalid
	}
	var out collab.EventPage
	err = c.request(ctx, "GET", "/v1/tasks/"+id+"/events?after="+url.QueryEscape(after)+"&limit="+strconv.Itoa(limit), nil, &out)
	return out, err
}

func (c *Client) StartTaskRun(ctx context.Context, id string, in collab.StartRunInput) (collab.Receipt, error) {
	if _, err := c.ReadTask(ctx, id); err != nil {
		return collab.Receipt{}, err
	}
	var extra []string
	if in.ExecutorModel != "" {
		extra = append(extra, "task_executor_model_v1")
	}
	if err := c.taskCapabilities(ctx, extra...); err != nil {
		return collab.Receipt{}, err
	}
	var out collab.Receipt
	err := c.request(ctx, "POST", "/v1/tasks/"+id+"/runs", in, &out)
	return out, err
}

func (c *Client) ReadReviewBundle(ctx context.Context, id, artifact string) (collab.ReviewBundle, error) {
	var out collab.ReviewBundle
	if !validID(artifact) {
		return out, collab.ErrInvalid
	}
	snap, err := c.ReadTask(ctx, id)
	if err != nil {
		return out, err
	}
	if err = c.taskCapabilities(ctx); err != nil {
		return out, err
	}
	found := false
	for _, ref := range snap.ArtifactRefs {
		if ref.ArtifactID == artifact && ref.Kind == "review_bundle" && ref.CaptureMode == "immutable" {
			found = true
		}
	}
	if !found {
		return out, collab.ErrNotFound
	}
	if err = c.request(ctx, "GET", "/v1/tasks/"+id+"/immutable-artifacts/"+artifact, nil, &out); err != nil {
		return out, err
	}
	c.mu.Lock()
	sid := c.id
	c.mu.Unlock()
	if out.Source.TaskID != id || out.Source.ServerID != c.binding.verifiedServerID || out.Source.StoreID != c.binding.verifiedStoreID {
		return collab.ReviewBundle{}, collab.ErrConflict
	}
	for _, attempt := range snap.Attempts {
		if attempt.AttemptID == out.AttemptID && attempt.SourceRef.SessionID == sid && attempt.SourceRef.TurnID == out.TurnID {
			return out, nil
		}
	}
	return collab.ReviewBundle{}, collab.ErrConflict
}

func (c *Client) ReviewTaskRun(ctx context.Context, id, runID string, in collab.ReviewInput) (collab.Receipt, error) {
	snap, err := c.ReadTask(ctx, id)
	if err != nil {
		return collab.Receipt{}, err
	}
	if snap.ManagedRun == nil || snap.ManagedRun.RunID != runID || in.RunID != runID {
		return collab.Receipt{}, collab.ErrConflict
	}
	if err = c.taskCapabilities(ctx); err != nil {
		return collab.Receipt{}, err
	}
	var out collab.Receipt
	err = c.request(ctx, "POST", "/v1/tasks/"+id+"/runs/"+runID+"/reviews", in, &out)
	return out, err
}
