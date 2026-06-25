// Package conversation is the runtime's programmatic API for one agent thread.
// It consolidates what the REPL and TUI each wired by hand — a Runner, its
// Session, per-turn cancellation, event fan-out, and autosave — behind one type
// the CLI, the server, macOS and iOS all drive identically. It knows nothing
// about transports (HTTP/WS) or UI; that is the job of the layers above it.
//
// Mapping to the intended Agent API: New ≈ StartConversation, SendMessage,
// Cancel, Subscribe ≈ SubscribeEvents.
package conversation

import (
	"context"
	"errors"
	"sync"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

// ErrBusy is returned by SendMessage when a turn is already in flight. A
// conversation runs one turn at a time; cancel the current one first.
var ErrBusy = errors.New("conversation: a turn is already in flight")

// turnRunner is the slice of *agent.Runner that a conversation drives. Defining
// it as an interface keeps the conversation unit-testable with a fake.
type turnRunner interface {
	RunTurn(ctx context.Context, sess *session.Session, userInput string) (agent.TurnResult, error)
}

// saver is the slice of session.Store the conversation needs: persist the
// session after a turn. A nil saver disables autosave.
type saver interface {
	Save(ctx context.Context, s *session.Session) error
}

// Conversation is the handle to one agent thread.
type Conversation struct {
	runner turnRunner
	sess   *session.Session
	store  saver
	hub    *hub

	// setApprover swaps the runner's Approver. Captured in New (the runner field
	// is the turnRunner interface, but the Approver lives on the concrete Runner).
	setApprover func(agent.Approver)

	mu     sync.Mutex
	cancel context.CancelFunc // non-nil while a turn is in flight

	// OnSaveError, if set, is invoked when the post-turn autosave fails. Autosave
	// is best-effort and never fails the turn; this only surfaces the warning
	// (the REPL prints it, a server might log it). Default nil = silent.
	OnSaveError func(error)
}

// New wraps an already-built Runner and Session. It intercepts the Runner's
// Emitter, preserving it as the hub's downstream sink (so persistence/telemetry
// keep working) while adding the multi-subscriber fan-out. store may be nil to
// disable autosave.
//
// "Starting" a conversation = build the session (session.NewBuilder) + build the
// Runner (buildRunner) + New. That assembly stays where config lives; this
// package deliberately does not depend on app config.
func New(runner *agent.Runner, sess *session.Session, store session.Store) *Conversation {
	h := newHub(runner.Emitter)
	runner.Emitter = h
	c := &Conversation{
		runner:      runner,
		sess:        sess,
		hub:         h,
		setApprover: func(a agent.Approver) { runner.Approver = a },
	}
	if store != nil {
		c.store = store
	}
	return c
}

// ID is the durable session id — the handle to resume this conversation.
func (c *Conversation) ID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.ID
}

// Session returns the underlying session (read-only use by callers).
func (c *Conversation) Session() *session.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

// SetSession swaps the active session — the REPL's /resume, which moves the
// thread to a different stored conversation. It must not be called while a turn
// is in flight. (Once a Conversation Manager owns one Conversation per session,
// /resume becomes a conversation switch and this affordance goes away.)
func (c *Conversation) SetSession(s *session.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess = s
}

// Subscribe returns a live event channel and an unsubscribe func. Each subscriber
// gets its own channel; delivery is non-blocking (see hub.Emit).
func (c *Conversation) Subscribe() (<-chan agent.Event, func()) {
	return c.hub.subscribe()
}

// SendMessage drives exactly one turn to completion, streaming events to
// subscribers and autosaving the session afterward (best-effort, when a store is
// set and the session is non-empty — mirroring the REPL/TUI). It rejects a
// concurrent turn with ErrBusy.
//
// The turn runs under a child context the conversation owns, so Cancel can stop
// it without canceling the caller's ctx. Save runs under the caller's ctx so a
// canceled turn is still persisted.
func (c *Conversation) SendMessage(ctx context.Context, text string) (agent.TurnResult, error) {
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		return agent.TurnResult{}, ErrBusy
	}
	sess := c.sess
	turnCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.mu.Unlock()

	res, err := c.runner.RunTurn(turnCtx, sess, text)

	cancel()
	c.mu.Lock()
	c.cancel = nil
	c.mu.Unlock()

	// Autosave runs even when the turn was canceled or errored: RunTurn appends
	// the user message before its first cancellation checkpoint, so the partial
	// history is consistent and resumable. WithoutCancel keeps the save itself
	// from being aborted by the turn's (or the caller's) cancellation.
	if c.store != nil && !sess.IsEmpty() {
		if serr := c.store.Save(context.WithoutCancel(ctx), sess); serr != nil && c.OnSaveError != nil {
			c.OnSaveError(serr)
		}
	}
	return res, err
}

// Cancel stops the in-flight turn at the next cancellation checkpoint and is a
// no-op when idle. The session is preserved.
func (c *Conversation) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
}

// Close cancels any in-flight turn and ends every live subscription — their
// channels close, so streaming bridges return. The conversation must not drive
// further turns after Close. Idempotent.
func (c *Conversation) Close() {
	c.Cancel()
	c.hub.closeAll()
}

// SetApprover swaps the approver the runner consults before side-effecting tools.
// A server connection attaches its remote approver here while it controls the
// conversation and restores a deny-all on detach. Must not be called while a turn
// is in flight (the loop reads the approver mid-turn).
func (c *Conversation) SetApprover(a agent.Approver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.setApprover != nil {
		c.setApprover(a)
	}
}
