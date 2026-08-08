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

// Deprecated: use approve.Granter instead.
type PermissionGranter = approve.Granter

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
// that becomes: send an approval_request frame to EVERY connected device, then
// wait for the matching approval_response (delivered via Resolve). A deadline
// denies; an empty sinks set (no client connected) blocks until a client
// reconnects and the request is re-sent via AddSink.
//
// Multi-device broadcast (§P.F): approval and ask_user requests are fanned out
// to ALL active WS connections, not just the latest. A user with both Mac and
// iPhone connected sees the dialog on both devices and can approve from either.
// Each connection's sink is keyed by its claimSessionControl revision, which is
// monotonic per-session and unique per-connection.
//
// RemoteApprover is session-scoped: it survives WebSocket disconnects so a user
// switching between conversations does not lose pending approvals. The owning
// WSHandler calls AddSink on connect and RemoveSink on disconnect; Close is
// reserved for session teardown (DELETE /v1/conversations/{id}).
type RemoteApprover struct {
	timeout time.Duration
	granter PermissionGranter // nil disables "always allow" persistence

	mu      sync.Mutex
	sinks   map[uint64]FrameSink // active connections, keyed by connection revision
	pending map[string]*pendingReq
	closed  bool

	// ask_user clarification requests use an independent map and mutex so the
	// different HITL mechanisms don't share data structures. When a client is
	// connected there is no fixed timeout — the turn waits for the user to
	// answer, which can be hours later. If every client disconnects a grace
	// period starts; the user can reconnect within it. Headless retains a
	// short fallback timeout.
	askUserMu          sync.Mutex
	askUserPending     map[string]*askUserReq
	askUserTimeout     time.Duration // headless fallback deadline (default 5m)
	askUserPoll        time.Duration // sink-presence poll interval (default 15s)
	askUserGracePeriod time.Duration // reconnection grace after last sink drops (default 10m)
}

var _ agent.Approver = (*RemoteApprover)(nil)

// NewRemoteApprover asks over sink. A non-positive timeout means "wait until
// resolved or closed" (rely on Close at session teardown rather than a deadline).
// granter (may be nil) persists a client's "always allow" grant into the shared
// permission store.
func NewRemoteApprover(timeout time.Duration, granter PermissionGranter) *RemoteApprover {
	return &RemoteApprover{
		timeout: timeout, granter: granter,
		sinks:          make(map[uint64]FrameSink),
		pending:        make(map[string]*pendingReq),
		askUserPending:     make(map[string]*askUserReq),
		askUserTimeout:     5 * time.Minute,
		askUserPoll:        15 * time.Second,
		askUserGracePeriod: 10 * time.Minute,
	}
}

// SetGranter replaces the permission granter for this approver. Called by the
// serve builder when a turn's workspace path is resolved, so "Always allow"
// grants persist to the workspace's .codeagent/settings.local.json instead of
// the user-global settings file (preventing workspace-to-workspace pollution).
func (a *RemoteApprover) SetGranter(g approve.Granter) {
	a.mu.Lock()
	a.granter = g
	a.mu.Unlock()
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
	sinks := make([]FrameSink, 0, len(a.sinks))
	for _, s := range a.sinks {
		sinks = append(sinks, s)
	}
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	// Broadcast to ALL connected devices. If no device is connected, the request
	// stays pending and will be re-sent when a client connects via AddSink.
	r := NewApprovalRequest(id, "", "", toolName, string(input), a.timeout.Milliseconds())
	frame, err := json.Marshal(r)
	if err != nil {
		return agent.VerdictDeny
	}
	for _, snk := range sinks {
		if err := snk.Send(frame); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[approver] %s send to sink failed: %v\n", toolName, err)
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
	sinks := make([]FrameSink, 0, len(a.sinks))
	for _, s := range a.sinks {
		sinks = append(sinks, s)
	}
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	r := NewApprovalRequest(id, "", "", "external_path_access", string(input), a.timeout.Milliseconds())
	frame, err := json.Marshal(r)
	if err != nil {
		return false
	}
	for _, snk := range sinks {
		if err := snk.Send(frame); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[approve] external_path_access send to sink failed: %v\n", err)
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

// --- ask_user types (independent of pendingReq — no field bloat) ---------

// askUserReq holds a single in-flight ask_user clarification request. It is
// stored in RemoteApprover.askUserPending (a separate map from pending) so the
// two HITL mechanisms — permission gates and task clarification — never share
// data structures. outcome (approved/always/scope) has no meaning here.
// question is a value copy and deadlineMS is frozen at creation so re-send in
// AddSink can replay the original wire form without recomputing.
type askUserReq struct {
	ch         chan askUserResult
	question   agent.AskUserQuestion
	deadlineMS int64 // frozen at creation; used by AddSink re-send
}

type askUserResult struct {
	answer   agent.AskUserAnswer
	timedOut bool
}

// AskUser implements agent.AskUserApprover. It sends an ask_user_request frame
// and blocks until the client responds or the session is closed. When at least
// one client is connected there is no fixed timeout — the turn waits until the
// user answers, which can be hours later. If every client disconnects, a grace
// period starts; the user can reconnect and answer within it. Headless (no
// sinks at all) retains a short fallback timeout so the process does not hang.
//
// Lock ordering: askUserMu (pending map), then mu (sink/closed). Close() takes
// mu then askUserMu — serial, not nested, so no deadlock.
func (a *RemoteApprover) AskUser(q agent.AskUserQuestion) (agent.AskUserAnswer, error) {
	id := newApprovalID()

	// Compute timeout once — shared by wire message and both wait paths.
	timeout := a.askUserTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	// Read sinks and closed under their own lock — they are written under mu,
	// not askUserMu. The wire deadline is derived from sink presence and
	// frozen into req BEFORE it is registered, so an AddSink re-send can
	// replay it without a data race (req becomes visible to AddSink only once
	// it is in askUserPending).
	a.mu.Lock()
	sinks := make([]FrameSink, 0, len(a.sinks))
	for _, s := range a.sinks {
		sinks = append(sinks, s)
	}
	a.mu.Unlock()

	// Wire deadline: omitted (0) when clients are connected (protocol says
	// "missing deadline_ms = no deadline"), present for headless fallback.
	// Frozen on req so an AddSink re-send replays an identical frame — no
	// disagreement between what the client sees and what the server enforces.
	deadlineMS := int64(0)
	if len(sinks) == 0 {
		deadlineMS = timeout.Milliseconds()
	}

	req := &askUserReq{
		ch:         make(chan askUserResult, 1),
		question:   q,
		deadlineMS: deadlineMS,
	}

	a.askUserMu.Lock()
	if a.askUserPending == nil {
		a.askUserPending = make(map[string]*askUserReq)
	}
	a.askUserPending[id] = req
	a.askUserMu.Unlock()

	// Re-check closed AFTER registration: Close() may have run since the
	// snapshot above and drained the map, in which case our request would
	// never receive its timedOut signal — detect it here and bail instead of
	// blocking on a request nobody can resolve.
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		a.askUserMu.Lock()
		delete(a.askUserPending, id)
		a.askUserMu.Unlock()
		return agent.AskUserAnswer{}, fmt.Errorf("approver closed")
	}

	defer func() {
		a.askUserMu.Lock()
		delete(a.askUserPending, id)
		a.askUserMu.Unlock()
	}()

	r := AskUserRequest{
		Type:       "ask_user_request",
		ID:         id,
		Question:   q,
		DeadlineMS: deadlineMS,
	}
	frame, err := json.Marshal(r)
	if err != nil {
		return agent.AskUserAnswer{}, err
	}
	for _, snk := range sinks {
		if err := snk.Send(frame); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[approver] ask_user send to sink failed: %v\n", err)
		}
	}

	if len(sinks) > 0 {
		return a.waitWithGracePeriod(req)
	}

	// Headless: wait with a short fallback timeout so the agent can move on.
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	select {
	case res := <-req.ch:
		if res.timedOut {
			return agent.AskUserAnswer{}, fmt.Errorf("ask_user timed out")
		}
		return res.answer, nil
	case <-deadline.C:
		return agent.AskUserAnswer{}, fmt.Errorf("ask_user timed out")
	}
}

// waitWithGracePeriod blocks until the user answers or the session is closed.
// While at least one client is connected there is no deadline — the user can
// return hours later and answer. If every client disconnects, a grace period
// starts; the user can reconnect within it. If the grace period expires
// without a reconnection the request times out, preventing a permanent hang.
func (a *RemoteApprover) waitWithGracePeriod(req *askUserReq) (agent.AskUserAnswer, error) {
	pollTicker := time.NewTicker(a.askUserPoll)
	defer pollTicker.Stop()

	var graceTimer *time.Timer
	var graceCh <-chan time.Time // nil channel blocks forever in select

	for {
		select {
		case res := <-req.ch:
			if graceTimer != nil {
				graceTimer.Stop()
			}
			if res.timedOut {
				return agent.AskUserAnswer{}, fmt.Errorf("approver closed")
			}
			return res.answer, nil

		case <-pollTicker.C:
			a.mu.Lock()
			sinksExist := len(a.sinks) > 0
			a.mu.Unlock()

			if sinksExist {
				// Client is connected — cancel any grace deadline.
				if graceTimer != nil {
					graceTimer.Stop()
					graceTimer = nil
					graceCh = nil
				}
			} else if graceTimer == nil {
				// Last client just disconnected — start grace period.
				graceTimer = time.NewTimer(a.askUserGracePeriod)
				graceCh = graceTimer.C
			}
			// If sinks are gone and grace timer is already running, let it tick.

		case <-graceCh:
			return agent.AskUserAnswer{}, fmt.Errorf("ask_user: no client reconnected within grace period")
		}
	}
}

// ResolveAskUser delivers the client's answer to a blocked AskUser call.
func (a *RemoteApprover) ResolveAskUser(id string, answer agent.AskUserAnswer) {
	a.askUserMu.Lock()
	req, ok := a.askUserPending[id]
	if ok {
		delete(a.askUserPending, id)
	}
	a.askUserMu.Unlock()
	if ok {
		select {
		case req.ch <- askUserResult{answer: answer}:
		default:
		}
	}
}

// Compile-time checks.
var (
	_ tools.PathAccessApprover = (*RemoteApprover)(nil)
	_ agent.AskUserApprover    = (*RemoteApprover)(nil)
)

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
	sinks := make([]FrameSink, 0, len(a.sinks))
	for _, s := range a.sinks {
		sinks = append(sinks, s)
	}
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

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
	for _, snk := range sinks {
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

// AddSink registers a device's FrameSink and re-sends every pending request to it
// so the newly connected client sees them immediately. Call on WebSocket connect.
// revision is the monotonic connection revision from claimSessionControl.
func (a *RemoteApprover) AddSink(revision uint64, sink FrameSink) {
	a.mu.Lock()
	a.sinks[revision] = sink
	pending := make([]*pendingReq, 0, len(a.pending))
	for id, req := range a.pending {
		pending = append(pending, req)
		_ = id
	}
	a.mu.Unlock()

	for _, req := range pending {
		a.mu.Lock()
		var reqID string
		for id, r := range a.pending {
			if r == req {
				reqID = id
				break
			}
		}
		snk := a.sinks[revision]
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

	// Re-send pending ask_user requests (independent loop — different data model).
	// Collect under askUserMu, then read sink under mu to avoid data race.
	a.askUserMu.Lock()
	askPending := make([]*askUserReq, 0, len(a.askUserPending))
	for id, req := range a.askUserPending {
		askPending = append(askPending, req)
		_ = id
	}
	a.askUserMu.Unlock()

	a.mu.Lock()
	snk := a.sinks[revision]
	a.mu.Unlock()

	for _, req := range askPending {
		a.askUserMu.Lock()
		var reqID string
		for id, r := range a.askUserPending {
			if r == req {
				reqID = id
				break
			}
		}
		a.askUserMu.Unlock()

		if reqID == "" || snk == nil {
			continue
		}
		// Re-use the deadline frozen at creation so the re-sent frame is
		// identical to the original — no disagreement between what the
		// client sees (DeadlineMS) and what the server enforces.
		r := AskUserRequest{
			Type:       "ask_user_request",
			ID:         reqID,
			Question:   req.question,
			DeadlineMS: req.deadlineMS,
		}
		if frame, err := json.Marshal(r); err == nil {
			if err := snk.Send(frame); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "[approver] ask_user re-send failed: %v\n", err)
			}
		}
	}
}

// ClearSink detaches the current sink without denying pending requests. The
// requests stay registered so they can be re-sent on the next UpdateSink. Call on
// RemoveSink detaches one device's FrameSink on disconnect. Pending requests stay
// registered so other connected devices can still approve. Call on WebSocket
// disconnect with the connection's revision.
func (a *RemoteApprover) RemoveSink(revision uint64) {
	a.mu.Lock()
	delete(a.sinks, revision)
	a.mu.Unlock()
}

// ClearSink removes every active sink — used as a compatibility shim by callers
// that don't track per-connection revisions. Prefer RemoveSink when possible.
func (a *RemoteApprover) ClearSink() {
	a.mu.Lock()
	a.sinks = make(map[uint64]FrameSink)
	a.mu.Unlock()
}

// Close denies every pending approval and ask_user request, and rejects future
// ones. Only called at session teardown (DELETE /v1/conversations/{id} or server
// shutdown), NOT on WebSocket disconnect.
//
// Lock ordering: mu, then askUserMu — serial, not nested. AskUser() takes
// askUserMu then mu in separate critical sections, so there is no deadlock risk.
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

	a.askUserMu.Lock()
	askPending := a.askUserPending
	a.askUserPending = make(map[string]*askUserReq)
	a.askUserMu.Unlock()
	for _, req := range askPending {
		select {
		case req.ch <- askUserResult{timedOut: true}:
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
