package conversation

import (
	"context"
	"errors"
	"sync"

	"code-agent/internal/agent"
)

// ErrBusy is returned by ActiveTurnRegistry.BeginTurn when a turn is already
// in flight for the given session.
var ErrBusy = errors.New("conversation: a turn is already in flight")

// ActiveTurnRegistry tracks currently-executing turns for mutual exclusion,
// cancellation, and approver wiring. Sessions live here only while a turn is
// in flight or a client is connected (approver set). It is deliberately
// minimal: no metrics, no durations, no loggers — those are separate concerns.
type ActiveTurnRegistry struct {
	mu       sync.Mutex
	turns    map[string]*activeTurn
	shutdown bool
}

type activeTurn struct {
	cancel           context.CancelFunc     // non-nil while a turn is in flight
	approver         agent.Approver         // set by WS handler; nil = deny-all
	planApprover     agent.PlanApprover     // set by WS handler; nil = auto-approve
	askUserApprover  agent.AskUserApprover  // set by WS handler; nil = headless fallback
	clientWaiter     agent.ClientToolWaiter // set by WS handler; nil = no client executor
	clientTools      []agent.ClientToolDef  // set by register_tools message; nil until registered

	// suspended marks that this in-flight turn was cancelled by SuspendAll (app
	// backgrounding), not by a user stop. The turn goroutine reads it as it unwinds
	// to record turn_status=paused rather than done (v1.2 §3). Reset by FinishTurn.
	suspended bool
	// done is closed by FinishTurn when the turn goroutine exits, so SuspendAll can
	// await a clean unwind (and its paused-status save) within a bounded window.
	done chan struct{}
}

// ClientTools returns the client-registered tools for a session (nil if none).
func (r *ActiveTurnRegistry) ClientTools(sessionID string) []agent.ClientToolDef {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[sessionID]
	if !ok {
		return nil
	}
	return t.clientTools
}

// RegisterTools stores client-side tool definitions for a session. Called when
// the client sends a register_tools message after the hello handshake.
func (r *ActiveTurnRegistry) RegisterTools(sessionID string, tools []agent.ClientToolDef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return
	}
	t, ok := r.turns[sessionID]
	if !ok {
		t = &activeTurn{}
		r.turns[sessionID] = t
	}
	t.clientTools = tools
}

// NewActiveTurnRegistry creates an empty registry.
func NewActiveTurnRegistry() *ActiveTurnRegistry {
	return &ActiveTurnRegistry{turns: make(map[string]*activeTurn)}
}

// BeginTurn reserves a session for exclusive execution. It returns a derived
// context (for cancellation) and its cancel func, or ErrBusy if another turn
// is already in flight for this session.
func (r *ActiveTurnRegistry) BeginTurn(sessionID string, parentCtx context.Context) (context.Context, context.CancelFunc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return nil, nil, errors.New("registry is shut down")
	}
	t, ok := r.turns[sessionID]
	if ok && t.cancel != nil {
		return nil, nil, ErrBusy
	}
	if !ok {
		t = &activeTurn{}
		r.turns[sessionID] = t
	}
	ctx, cancel := context.WithCancel(parentCtx)
	t.cancel = cancel
	t.suspended = false
	t.done = make(chan struct{})
	return ctx, cancel, nil
}

// FinishTurn releases the session's turn slot. If no connection-owned state
// remains (approver, plan approver, client waiter), the entry is cleaned up.
func (r *ActiveTurnRegistry) FinishTurn(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[sessionID]
	if !ok {
		return
	}
	t.cancel = nil
	t.suspended = false
	if t.done != nil {
		close(t.done) // wake any SuspendAll awaiting this turn's unwind
		t.done = nil
	}
	if t.approver == nil && t.planApprover == nil && t.askUserApprover == nil && t.clientWaiter == nil {
		delete(r.turns, sessionID)
	}
}

// SuspendAll cancels every in-flight turn as an app-suspend (not a user stop),
// then waits — bounded by ctx — for each to unwind so its turn_status=paused save
// lands. It returns the session ids it suspended. Correctness does not depend on
// the await completing: the per-iteration Checkpointer has already persisted a
// consistent history, so ctx expiring (the host's watchdog) only means the paused
// label may lag, never that the session is unresumable (v1.2 §2.2.1/§3.1).
func (r *ActiveTurnRegistry) SuspendAll(ctx context.Context) []string {
	r.mu.Lock()
	var ids []string
	var dones []chan struct{}
	for id, t := range r.turns {
		if t.cancel != nil {
			t.suspended = true
			t.cancel()
			ids = append(ids, id)
			if t.done != nil {
				dones = append(dones, t.done)
			}
		}
	}
	r.mu.Unlock()

	for _, d := range dones {
		select {
		case <-d:
		case <-ctx.Done():
			return ids
		}
	}
	return ids
}

// WasSuspended reports whether the session's current (or just-finished) turn was
// cancelled by SuspendAll. The turn goroutine consults it while unwinding to
// choose between turn_status paused (suspend) and done (normal/user-cancel).
func (r *ActiveTurnRegistry) WasSuspended(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[sessionID]
	return ok && t.suspended
}

// Cancel stops the in-flight turn for the given session at the next checkpoint.
// A no-op when idle.
func (r *ActiveTurnRegistry) Cancel(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[sessionID]
	if !ok || t.cancel == nil {
		return
	}
	t.cancel()
}

// Approver returns the approver currently set for a session (nil = deny-all).
func (r *ActiveTurnRegistry) Approver(sessionID string) agent.Approver {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[sessionID]
	if !ok {
		return nil
	}
	return t.approver
}

// SetApprover associates (or clears) an approver for a session. The WS handler
// sets a RemoteApprover on connect and a deny-all on disconnect.
func (r *ActiveTurnRegistry) SetApprover(sessionID string, a agent.Approver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return
	}
	t, ok := r.turns[sessionID]
	if !ok {
		t = &activeTurn{}
		r.turns[sessionID] = t
	}
	t.approver = a
	// If all connection-owned state is cleared and no turn is active, clean up.
	if a == nil && t.planApprover == nil && t.clientWaiter == nil && t.cancel == nil {
		delete(r.turns, sessionID)
	}
}

// PlanApprover returns the plan approver for a session (nil = auto-approve).
func (r *ActiveTurnRegistry) PlanApprover(sessionID string) agent.PlanApprover {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[sessionID]
	if !ok {
		return nil
	}
	return t.planApprover
}

// SetPlanApprover associates (or clears) a plan approver for a session. The WS
// handler sets the same RemoteApprover for both tool and plan approval.
func (r *ActiveTurnRegistry) SetPlanApprover(sessionID string, pa agent.PlanApprover) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return
	}
	t, ok := r.turns[sessionID]
	if !ok {
		t = &activeTurn{}
		r.turns[sessionID] = t
	}
	t.planApprover = pa
	// If all connection-owned state is cleared and no turn is active, clean up.
	if pa == nil && t.approver == nil && t.clientWaiter == nil && t.cancel == nil {
		delete(r.turns, sessionID)
	}
}

// AskUserApprover returns the ask_user approver for a session (nil = headless).
func (r *ActiveTurnRegistry) AskUserApprover(sessionID string) agent.AskUserApprover {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[sessionID]
	if !ok {
		return nil
	}
	return t.askUserApprover
}

// SetAskUserApprover associates (or clears) an ask_user approver for a session.
// The WS handler sets the same RemoteApprover as for tool and plan approval.
func (r *ActiveTurnRegistry) SetAskUserApprover(sessionID string, aa agent.AskUserApprover) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return
	}
	t, ok := r.turns[sessionID]
	if !ok {
		t = &activeTurn{}
		r.turns[sessionID] = t
	}
	t.askUserApprover = aa
	// If all connection-owned state is cleared and no turn is active, clean up.
	if aa == nil && t.approver == nil && t.planApprover == nil && t.clientWaiter == nil && t.cancel == nil {
		delete(r.turns, sessionID)
	}
}

// ClientToolWaiter returns the client tool waiter for a session (nil = no
// client executor).
func (r *ActiveTurnRegistry) ClientToolWaiter(sessionID string) agent.ClientToolWaiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[sessionID]
	if !ok {
		return nil
	}
	return t.clientWaiter
}

// SetClientToolWaiter associates (or clears) a client tool waiter for a session.
func (r *ActiveTurnRegistry) SetClientToolWaiter(sessionID string, w agent.ClientToolWaiter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return
	}
	t, ok := r.turns[sessionID]
	if !ok {
		t = &activeTurn{}
		r.turns[sessionID] = t
	}
	t.clientWaiter = w
	// If all connection-owned state is cleared and no turn is active, clean up.
	if w == nil && t.approver == nil && t.planApprover == nil && t.cancel == nil {
		delete(r.turns, sessionID)
	}
}

// Shutdown cancels all active turns, cancels all pending client tool calls, and
// rejects future operations.
func (r *ActiveTurnRegistry) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shutdown = true
	for _, t := range r.turns {
		if t.clientWaiter != nil {
			t.clientWaiter.CancelAll()
		}
		if t.cancel != nil {
			t.cancel()
		}
		if t.done != nil {
			close(t.done) // release any SuspendAll awaiting this turn
			t.done = nil
		}
	}
	r.turns = nil
}
