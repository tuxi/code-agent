package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Observation is a separate access mode from execution. It reads a completed
// Session against a recorded commit range, so it never requires the workspace
// to still sit on the task baseline. The execution binding keeps its original
// HEAD == baseline invariant; this file must not weaken it.
type Observation struct {
	SessionID     string `json:"session_id"`
	Baseline      string `json:"baseline_commit"`
	Delivery      string `json:"delivery_commit"`
	Workspace     string `json:"workspace_path"`
	EvidenceRange string `json:"evidence_range"`
}

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func validateCommitRange(ctx context.Context, root, baseline, delivery string) error {
	if !fullCommitPattern.MatchString(baseline) || !fullCommitPattern.MatchString(delivery) {
		return errors.New("baseline and delivery commits must be full commit SHAs")
	}
	for name, commit := range map[string]string{"baseline": baseline, "delivery": delivery} {
		if _, _, err := gitOutput(ctx, root, "cat-file", "-e", commit+"^{commit}"); err != nil {
			return fmt.Errorf("%s commit is not present in the bound repository", name)
		}
	}
	if baseline == delivery {
		return errors.New("delivery commit equals the baseline; there is no delivered change to review")
	}
	if _, _, err := gitOutput(ctx, root, "merge-base", "--is-ancestor", baseline, delivery); err != nil {
		return errors.New("baseline is not an ancestor of the delivery commit; the range would mix unrelated history")
	}
	return nil
}

// NewObserverFromManifest binds a completed Session to a recorded commit range.
// It never creates a Session, opens a control socket, or submits anything.
func NewObserverFromManifest(ctx context.Context, path, workspace, sessionID, baseline, delivery string) (*Client, ConnectionInfo, Observation, error) {
	var obs Observation
	if !validID(sessionID) {
		return nil, ConnectionInfo{}, obs, errors.New("a valid session_id is required for observation")
	}
	c, info, err := newFromManifest(ctx, path, workspace, baseline, delivery)
	if err != nil {
		return nil, info, obs, err
	}
	if info.StoreID == "" {
		c.Close()
		return nil, info, obs, errors.New("observation requires a verified store identity")
	}
	if err = c.observe(ctx, sessionID); err != nil {
		c.Close()
		return nil, info, obs, fmt.Errorf("session_observation: %w", err)
	}
	obs = Observation{SessionID: sessionID, Baseline: c.baseline, Delivery: c.delivery, Workspace: c.root, EvidenceRange: c.baseline + ".." + c.delivery}
	return c, info, obs, nil
}

// observe binds an existing Session for reading only. Unlike reattach it does
// not recover admissions, require an idle queue, or reconnect a control socket:
// none of those are needed to read durable events and a commit range.
func (c *Client) observe(ctx context.Context, id string) error {
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
	// Report the actual mismatch. Paths and policy are not secret, and a vague
	// "does not match" leaves the operator guessing which binding is wrong.
	var mismatch []string
	if detail.ID != id {
		mismatch = append(mismatch, "id="+detail.ID)
	}
	if detail.Workspace != c.root {
		mismatch = append(mismatch, "workspace_path="+detail.Workspace)
	}
	if detail.Policy != "shared_workspace" {
		mismatch = append(mismatch, "execution_policy="+detail.Policy)
	}
	if detail.Archived != "" {
		mismatch = append(mismatch, "archived_at="+detail.Archived)
	}
	if detail.Rebind {
		mismatch = append(mismatch, "needs_rebind=true")
	}
	if detail.Fork != "" {
		mismatch = append(mismatch, "fork_source_session_id="+detail.Fork)
	}
	if len(mismatch) > 0 {
		return fmt.Errorf("existing session does not match the bound workspace (bridge root=%s): %s; pass --workspace with the session's own workspace_path", c.root, strings.Join(mismatch, " "))
	}
	c.mu.Lock()
	c.id, c.observeOnly = id, true
	c.mu.Unlock()
	return nil
}

// rangeDiff is the reviewable change itself: baseline..delivery. It is stable
// regardless of the current HEAD, branch, or worktree state.
func (c *Client) rangeDiff(ctx context.Context) (string, bool, error) {
	diff, truncated, err := gitOutput(ctx, c.root, "diff", "--no-ext-diff", "--no-textconv", c.baseline, c.delivery, "--")
	if err != nil {
		return "", false, err
	}
	if len(diff) > maxGitOutput {
		return diff[:maxGitOutput], true, nil
	}
	return diff, truncated, nil
}

func (c *Client) rangeFiles(ctx context.Context) ([]string, bool, error) {
	names, cut, err := gitOutput(ctx, c.root, "diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", c.baseline, c.delivery, "--")
	if err != nil {
		return nil, false, err
	}
	files := map[string]bool{}
	if cut {
		if at := strings.LastIndexByte(names, 0); at >= 0 {
			names = names[:at+1]
		} else {
			names = ""
		}
	}
	for _, name := range strings.Split(names, "\x00") {
		if name != "" {
			files[name] = true
		}
	}
	out := make([]string, 0, len(files))
	for name := range files {
		out = append(out, name)
	}
	return out, cut, nil
}

// headState reports where the workspace is now. It is reported, never used to
// substitute for the recorded delivery commit.
func (c *Client) headState(ctx context.Context) (string, bool) {
	head, _, err := gitOutput(ctx, c.root, "rev-parse", "HEAD")
	if err != nil {
		return "", false
	}
	status, truncated, err := gitOutput(ctx, c.root, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return strings.TrimSpace(head), false
	}
	return strings.TrimSpace(head), truncated || strings.TrimSpace(status) != ""
}

// BoundSessionID exposes the single Session this process is bound to.
func (c *Client) BoundSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id
}

func (c *Client) ObservationInfo() Observation {
	return Observation{SessionID: c.BoundSessionID(), Baseline: c.baseline, Delivery: c.delivery, Workspace: c.root, EvidenceRange: c.baseline + ".." + c.delivery}
}
