package server

import (
	"context"
	"sync"
	"time"

	"code-agent/internal/agent"
)

// RemoteToolResultWaiter implements agent.ClientToolWaiter for a single WS
// connection. Its concurrency model mirrors RemoteApprover exactly: a
// mutex-protected map of per-callID channels; Wait reads from a channel,
// Deliver writes to it.
//
// See docs/protocols/agent-wire-v1.1-client-tool-execution.md §4.
type RemoteToolResultWaiter struct {
	mu      sync.Mutex
	pending map[string]*pendingToolCall
}

type pendingToolCall struct {
	ch        chan agent.ToolCallResult
	done      chan struct{} // closed when Wait returns; unblocks CancelAll
	closeOnce sync.Once     // ensures done is closed exactly once
}

// compile-time check
var _ agent.ClientToolWaiter = (*RemoteToolResultWaiter)(nil)

// NewRemoteToolResultWaiter creates a waiter with no pending calls.
func NewRemoteToolResultWaiter() *RemoteToolResultWaiter {
	return &RemoteToolResultWaiter{pending: make(map[string]*pendingToolCall)}
}

// Wait blocks until a result is delivered, the lease expires, the call is
// cancelled via done, or ctx is done. It always cleans up its entry in the
// pending map on return.
func (w *RemoteToolResultWaiter) Wait(ctx context.Context, callID string, leaseTimeout time.Duration) (agent.ToolCallResult, error) {
	pc := &pendingToolCall{
		ch:   make(chan agent.ToolCallResult, 1),
		done: make(chan struct{}),
	}
	w.mu.Lock()
	w.pending[callID] = pc
	w.mu.Unlock()

	defer func() {
		pc.closeOnce.Do(func() { close(pc.done) })
		w.mu.Lock()
		delete(w.pending, callID)
		w.mu.Unlock()
	}()

	timer := time.NewTimer(leaseTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return agent.ToolCallResult{}, ctx.Err()
	case <-timer.C:
		return agent.ToolCallResult{Subtype: "result", Content: "tool error: client timeout", IsError: true}, nil
	case <-pc.done:
		return agent.ToolCallResult{Subtype: "result", Content: "tool error: connection lost", IsError: true}, nil
	case r := <-pc.ch:
		return r, nil
	}
}

// Deliver resolves a pending Wait. Unknown callID is silently dropped (the call
// may have already timed out or completed). Delivery after Wait has returned
// (timeout / cancel) is also silently dropped — the done channel guards against
// writing to a channel with no reader.
func (w *RemoteToolResultWaiter) Deliver(callID string, result agent.ToolCallResult) {
	w.mu.Lock()
	pc, ok := w.pending[callID]
	w.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-pc.done:
		return // Wait already returned; don't block the Deliver caller
	case pc.ch <- result:
	}
}

// CancelAll wakes every pending Wait by closing their done channels. Each
// blocked Wait unblocks via the done case in its select, returning a
// connection-lost error. The pending map is cleared; subsequent Deliver calls
// are no-ops. Safe to call multiple times.
func (w *RemoteToolResultWaiter) CancelAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, pc := range w.pending {
		pc.closeOnce.Do(func() { close(pc.done) })
	}
	w.pending = nil
}
