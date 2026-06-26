package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"code-agent/internal/agent"
)

// RemoteApprover implements agent.Approver by asking a connected client. The
// agent loop calls Approve synchronously and blocks on the verdict; over the wire
// that becomes: send an approval_request frame, then wait for the matching
// approval_response (delivered via Resolve). A deadline or a closed approver
// (client gone) denies — the fail-safe "no answer = no" rule, the same default as
// a nil Approver in the loop.
//
// This is the one place the protocol's blocking, bidirectional control round-trip
// is reconciled with an async event stream: Approve runs on the turn goroutine and
// parks on a per-request channel; the connection's read loop runs Resolve on
// another goroutine to unpark it.
type RemoteApprover struct {
	sink    FrameSink
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]chan bool
	closed  bool
}

var _ agent.Approver = (*RemoteApprover)(nil)

// NewRemoteApprover asks over sink. A non-positive timeout means "wait until
// resolved or closed" (rely on Close at disconnect rather than a deadline).
func NewRemoteApprover(sink FrameSink, timeout time.Duration) *RemoteApprover {
	return &RemoteApprover{sink: sink, timeout: timeout, pending: make(map[string]chan bool)}
}

// Approve sends an approval_request and blocks until the verdict arrives, the
// deadline elapses, or the approver is closed. It denies on any path other than
// an explicit approval.
func (a *RemoteApprover) Approve(toolName string, input json.RawMessage) bool {
	id := newApprovalID()
	ch := make(chan bool, 1)

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return false
	}
	a.pending[id] = ch
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	req := NewApprovalRequest(id, "", "", toolName, string(input), a.timeout.Milliseconds())
	frame, err := json.Marshal(req)
	if err != nil {
		return false
	}
	if err := a.sink.Send(frame); err != nil {
		return false // cannot ask the client: deny
	}

	var deadline <-chan time.Time
	if a.timeout > 0 {
		t := time.NewTimer(a.timeout)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case approved := <-ch:
		return approved
	case <-deadline:
		return false // no answer in time: deny
	}
}

// Resolve delivers a client's verdict to the blocked Approve call with the
// matching id. Unknown or already-resolved ids are ignored.
func (a *RemoteApprover) Resolve(id string, approved bool) {
	a.mu.Lock()
	ch, ok := a.pending[id]
	a.mu.Unlock()
	if ok {
		select {
		case ch <- approved:
		default:
		}
	}
}

// ApprovePlan implements agent.PlanApprover by sending a plan_approval_request
// and blocking until the client responds. Same fail-safe as Approve: any path
// other than an explicit approval is a denial.
func (a *RemoteApprover) ApprovePlan(plan agent.Plan) agent.PlanDecision {
	id := newApprovalID()
	ch := make(chan bool, 1)

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return agent.PlanRejected
	}
	a.pending[id] = ch
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	req := PlanApprovalRequest{
		Type:       "plan_approval_request",
		ID:         id,
		PlanID:     plan.ID,
		Title:      plan.Title,
		Content:    plan.Content,
		DeadlineMS: a.timeout.Milliseconds(),
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return agent.PlanRejected
	}
	if err := a.sink.Send(frame); err != nil {
		return agent.PlanRejected
	}

	var deadline <-chan time.Time
	if a.timeout > 0 {
		t := time.NewTimer(a.timeout)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case approved := <-ch:
		if approved {
			return agent.PlanApproved
		}
		return agent.PlanRejected
	case <-deadline:
		return agent.PlanRejected
	}
}

var _ agent.PlanApprover = (*RemoteApprover)(nil)

// Close denies every pending approval and rejects future ones, so a turn waiting
// on a vanished client stops immediately instead of hanging to its deadline.
func (a *RemoteApprover) Close() {
	a.mu.Lock()
	a.closed = true
	pending := a.pending
	a.pending = make(map[string]chan bool)
	a.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- false:
		default:
		}
	}
}

// denyApprover refuses every side-effecting call. It is installed when no client
// controls a conversation (fail-safe default).
type denyApprover struct{}

func (denyApprover) Approve(string, json.RawMessage) bool { return false }

func newApprovalID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "appr_" + hex.EncodeToString(b[:])
}
