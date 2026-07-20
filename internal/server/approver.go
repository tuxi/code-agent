package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/approve"
	"code-agent/internal/tools"
)

// PermissionGranter persists an "always allow" grant for a tool at a scope. It is
// the narrow slice of *approve.RuleStore the RemoteApprover needs, injected so a
// wire client's "always" verdict adds a rule to the same store the loop's
// allowlist reads from. Nil disables persistence (an "always" is treated as a
// one-time allow).
type PermissionGranter interface {
	GrantTool(toolName string, scope approve.Scope) (rule string, err error)
}

// outcome is a client's three-way verdict delivered to a blocked Approve call.
type outcome struct {
	approved bool
	always   bool // persist a rule (tool approval only)
	scope    approve.Scope
}

// pendingReq holds a single in-flight approval or plan-approval request. It keeps
// the request data so it can be re-sent when a client reconnects after a disconnect.
type pendingReq struct {
	ch       chan outcome    // wakes the blocked Approve/ApprovePlan goroutine
	toolName string          // tool approval only
	input    json.RawMessage // tool arguments (tool approval only)
	plan     *agent.Plan     // plan approval only (nil for tool approval)
}

// RemoteApprover implements agent.Approver by asking a connected client. The
// agent loop calls Approve synchronously and blocks on the verdict; over the wire
// that becomes: send an approval_request frame, then wait for the matching
// approval_response (delivered via Resolve). A deadline denies; a nil sink (no
// client connected) blocks until a client reconnects and the request is re-sent.
//
// This is the one place the protocol's blocking, bidirectional control round-trip
// is reconciled with an async event stream: Approve runs on the turn goroutine and
// parks on a per-request channel; the connection's read loop runs Resolve on
// another goroutine to unpark it.
//
// RemoteApprover is session-scoped: it survives WebSocket disconnects so a user
// switching between conversations does not lose pending approvals. The owning
// WSHandler calls UpdateSink on connect and ClearSink on disconnect; Close is
// reserved for session teardown (DELETE /v1/conversations/{id}).
type RemoteApprover struct {
	timeout time.Duration
	granter PermissionGranter // nil disables "always allow" persistence

	mu      sync.Mutex
	sink    FrameSink // nil when no client connected; mutable via UpdateSink/ClearSink
	pending map[string]*pendingReq
	closed  bool
}

var _ agent.Approver = (*RemoteApprover)(nil)

// NewRemoteApprover asks over sink. A non-positive timeout means "wait until
// resolved or closed" (rely on Close at session teardown rather than a deadline).
// granter (may be nil) persists a client's "always allow" grant into the shared
// permission store.
func NewRemoteApprover(sink FrameSink, timeout time.Duration, granter PermissionGranter) *RemoteApprover {
	return &RemoteApprover{sink: sink, timeout: timeout, granter: granter, pending: make(map[string]*pendingReq)}
}

// Approve sends an approval_request and blocks until the verdict arrives, the
// deadline elapses, or the approver is closed. When no client is connected (sink
// is nil) the send is skipped — the request waits in pending until UpdateSink
// re-sends it. It denies on any path other than an explicit approval.
func (a *RemoteApprover) Approve(toolName string, input json.RawMessage) agent.Verdict {
	id := newApprovalID()
	req := &pendingReq{
		ch:       make(chan outcome, 1),
		toolName: toolName,
		input:    input,
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return agent.VerdictDeny
	}
	a.pending[id] = req
	snk := a.sink // capture under lock
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	// Send to current sink (if any). If sink is nil, the request stays pending
	// and will be re-sent when a client connects via UpdateSink.
	if snk != nil {
		r := NewApprovalRequest(id, "", "", toolName, string(input), a.timeout.Milliseconds())
		frame, err := json.Marshal(r)
		if err != nil {
			return agent.VerdictDeny
		}
		// A send failure means the client disconnected mid-send. Don't deny —
		// the request stays registered and will be re-sent on the next UpdateSink.
		if err := snk.Send(frame); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[approver] %s send failed: %v\n", toolName, err)
		}
	}

	var deadline <-chan time.Time
	if a.timeout > 0 {
		t := time.NewTimer(a.timeout)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case res := <-req.ch:
		// "Always allow": persist a rule (best-effort) so future matching calls
		// skip the prompt. The grant lands in the shared store the loop's allowlist
		// reads, so it takes effect on the very next call.
		if res.always && a.granter != nil {
			if rule, err := a.granter.GrantTool(toolName, res.scope); err != nil {
				fmt.Fprintf(os.Stderr, "[permissions] could not persist always-allow for %s: %v\n", toolName, err)
			} else {
				fmt.Fprintf(os.Stderr, "[permissions] always allowing %q (%s)\n", rule, scopeLabel(res.scope))
			}
		}
		if res.approved {
			return agent.VerdictAllow
		}
		return agent.VerdictDeny
	case <-deadline:
		return agent.VerdictDeny // no answer in time: deny
	}
}

// ApproveExternalPath implements tools.PathAccessApprover. It sends an
// approval_request with tool_name "external_path_access" and blocks until the
// client responds. The client renders the path and operation so the user can
// decide whether to grant read-only access outside the workspace.
//
// Unlike Approve, there is no "always allow" persistence for external path
// access — the user must approve each external path independently.
func (a *RemoteApprover) ApproveExternalPath(absolutePath string, operation string) bool {
	id := newApprovalID()
	input := json.RawMessage(fmt.Sprintf(`{"path":%q,"operation":%q}`, absolutePath, operation))
	req := &pendingReq{
		ch:       make(chan outcome, 1),
		toolName: "external_path_access",
		input:    input,
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return false
	}
	a.pending[id] = req
	snk := a.sink // capture under lock
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	if snk != nil {
		r := NewApprovalRequest(id, "", "", "external_path_access", string(input), a.timeout.Milliseconds())
		frame, err := json.Marshal(r)
		if err != nil {
			return false
		}
		if err := snk.Send(frame); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[approve] external_path_access send failed: %v\n", err)
			return false
		}
	}

	var deadline <-chan time.Time
	if a.timeout > 0 {
		t := time.NewTimer(a.timeout)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case res := <-req.ch:
		return res.approved
	case <-deadline:
		return false
	}
}

// Compile-time check: RemoteApprover implements tools.PathAccessApprover.
var _ tools.PathAccessApprover = (*RemoteApprover)(nil)

// Resolve delivers a plain approve/deny verdict (plan approvals, and legacy
// clients that send only `approved`). Unknown or already-resolved ids are ignored.
func (a *RemoteApprover) Resolve(id string, approved bool) {
	a.deliver(id, outcome{approved: approved})
}

// ResolveTool delivers a tool approval's three-way verdict, including whether to
// persist an "always allow" rule and at what scope.
func (a *RemoteApprover) ResolveTool(id string, approved, always bool, scope approve.Scope) {
	a.deliver(id, outcome{approved: approved, always: always, scope: scope})
}

func (a *RemoteApprover) deliver(id string, res outcome) {
	a.mu.Lock()
	req, ok := a.pending[id]
	if ok {
		// Resolution becomes visible to the attention snapshot before the blocked
		// goroutine is woken. Removing under the same lock also makes duplicate
		// verdicts harmless.
		delete(a.pending, id)
	}
	a.mu.Unlock()
	if ok {
		select {
		case req.ch <- res:
		default:
		}
	}
}

// PendingCount returns the number of unresolved approval requests. It is an
// attention fact only; request arguments remain on the session channel.
func (a *RemoteApprover) PendingCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

func scopeLabel(s approve.Scope) string {
	if s == approve.ScopeUser {
		return "user"
	}
	return "project-local"
}

// ApprovePlan implements agent.PlanApprover by sending a plan_approval_request
// and blocking until the client responds. Same session-scoped semantics as
// Approve: a nil sink skips the send and waits for UpdateSink to re-send.
func (a *RemoteApprover) ApprovePlan(plan agent.Plan) agent.PlanDecision {
	id := newApprovalID()
	req := &pendingReq{
		ch:   make(chan outcome, 1),
		plan: &plan,
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return agent.PlanRejected
	}
	a.pending[id] = req
	snk := a.sink // capture under lock
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	if snk != nil {
		r := PlanApprovalRequest{
			Type:       "plan_approval_request",
			ID:         id,
			PlanID:     plan.ID,
			Title:      plan.Title,
			PlanPath:   plan.WorkspaceRelativePath,
			FilePath:   plan.FilePath,
			Content:    plan.Content,
			DeadlineMS: a.timeout.Milliseconds(),
		}
		frame, err := json.Marshal(r)
		if err != nil {
			return agent.PlanRejected
		}
		// Send failure is not a denial — the request stays pending.
		_ = snk.Send(frame)
	}

	var deadline <-chan time.Time
	if a.timeout > 0 {
		t := time.NewTimer(a.timeout)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case res := <-req.ch:
		if res.approved {
			return agent.PlanApproved
		}
		return agent.PlanRejected
	case <-deadline:
		return agent.PlanRejected
	}
}

var _ agent.PlanApprover = (*RemoteApprover)(nil)

// UpdateSink sets a new (or replacement) FrameSink and re-sends every pending
// request so a reconnected client sees them. Call on WebSocket connect.
func (a *RemoteApprover) UpdateSink(sink FrameSink) {
	a.mu.Lock()
	a.sink = sink
	pending := make([]*pendingReq, 0, len(a.pending))
	for id, req := range a.pending {
		// Stash the id in the channel buffer slot so we can correlate (safe:
		// pendingReq.ch has capacity 1). We use a separate list because id is
		// the map key and we need both id and req for re-sending.
		pending = append(pending, req)
		_ = id // keep key alive
	}
	a.mu.Unlock()

	for _, req := range pending {
		// Re-send under a fresh lock to get the sink reference and the id.
		a.mu.Lock()
		// Find the id for this request.
		var reqID string
		for id, r := range a.pending {
			if r == req {
				reqID = id
				break
			}
		}
		snk := a.sink
		a.mu.Unlock()

		if reqID == "" || snk == nil {
			continue
		}

		if req.plan != nil {
			r := PlanApprovalRequest{
				Type:       "plan_approval_request",
				ID:         reqID,
				PlanID:     req.plan.ID,
				Title:      req.plan.Title,
				PlanPath:   req.plan.WorkspaceRelativePath,
				FilePath:   req.plan.FilePath,
				Content:    req.plan.Content,
				DeadlineMS: a.timeout.Milliseconds(),
			}
			if frame, err := json.Marshal(r); err == nil {
				_ = snk.Send(frame)
			}
		} else {
			r := NewApprovalRequest(reqID, "", "", req.toolName, string(req.input), a.timeout.Milliseconds())
			if frame, err := json.Marshal(r); err == nil {
				_ = snk.Send(frame)
			}
		}
	}
}

// ClearSink detaches the current sink without denying pending requests. The
// requests stay registered so they can be re-sent on the next UpdateSink. Call on
// WebSocket disconnect.
func (a *RemoteApprover) ClearSink() {
	a.mu.Lock()
	a.sink = nil
	a.mu.Unlock()
}

// Close denies every pending approval and rejects future ones. Only called at
// session teardown (DELETE /v1/conversations/{id} or server shutdown), NOT on
// WebSocket disconnect — the point of this type is to survive reconnects.
func (a *RemoteApprover) Close() {
	a.mu.Lock()
	a.closed = true
	pending := a.pending
	a.pending = make(map[string]*pendingReq)
	a.mu.Unlock()
	for _, req := range pending {
		select {
		case req.ch <- outcome{approved: false}:
		default:
		}
	}
}

// denyApprover refuses every side-effecting call. It is installed when no client
// controls a conversation (fail-safe default).
type denyApprover struct{}

func (denyApprover) Approve(string, json.RawMessage) agent.Verdict { return agent.VerdictDeny }

func newApprovalID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "appr_" + hex.EncodeToString(b[:])
}
