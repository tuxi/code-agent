package conversation

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/model"
	"code-agent/internal/session"
)

// TurnExecutor is the single execution entry point for all transports (WS,
// HTTP, REPL, TUI, webhook, cron). It orchestrates one turn:
//
//	Load → BeginTurn → Build Runtime → Run → Save → FinishTurn
//
// It does NOT own the Runner — the Runtime is built per-turn and discarded.
// It does NOT build the Runner itself — that is RuntimeFactory's job.
type TurnExecutor struct {
	repo   ConversationRepository
	events ConversationEventStore
	active *ActiveTurnRegistry
	subs   *SubscriptionManager
	rb     RunBuilder

	// titleGen, if set, asynchronously generates a human-readable title after the
	// first turn completes. Nil means no auto-titling (tests, headless).
	titleGen TitleGenerator

	// OnSaveError, if set, is invoked when the post-turn save fails. Autosave
	// is best-effort and never fails the turn; this only surfaces the warning
	// (the REPL prints it, a server logs it). Default nil = silent.
	OnSaveError func(error)
}

// SetTitleGenerator configures optional auto-titling. Separate from the
// constructor to keep the mandatory dependencies clear.
func (e *TurnExecutor) SetTitleGenerator(g TitleGenerator) {
	e.titleGen = g
}

// NewTurnExecutor wires the execution pipeline.
func NewTurnExecutor(repo ConversationRepository, events ConversationEventStore, active *ActiveTurnRegistry, subs *SubscriptionManager, rb RunBuilder) *TurnExecutor {
	return &TurnExecutor{repo: repo, events: events, active: active, subs: subs, rb: rb}
}

// Execute drives one turn to completion against the session identified by
// sessionID. It loads the session from the Repository, claims the turn slot,
// builds a fresh Runner, runs, saves, and releases the slot.
func (e *TurnExecutor) Execute(ctx context.Context, sessionID string, input string) (agent.TurnResult, error) {
	sess, err := e.repo.Load(ctx, sessionID)
	if err != nil {
		return agent.TurnResult{}, err
	}
	return e.ExecuteWithSession(ctx, sess, input)
}

// ExecuteWithSession runs a turn against an already-loaded session — the REPL
// and TUI path, where the caller holds a session handle across turns.
func (e *TurnExecutor) ExecuteWithSession(parentCtx context.Context, sess *session.Session, input string) (agent.TurnResult, error) {
	// 1. Claim the turn (mutual exclusion).
	turnCtx, cancel, err := e.active.BeginTurn(sess.ID, parentCtx)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer func() {
		cancel()
		e.active.FinishTurn(sess.ID)
	}()

	// 2. Assemble the composite publisher: persist events + fan out to live subscribers.
	pub := compositeEmitter{
		// Persist every non-ephemeral event to the EventStore.
		&eventStoreEmitter{ctx: parentCtx, events: e.events},
		// Fan out to WS/TUI subscribers (non-blocking).
		e.subs.Emitter(sess.ID),
	}

	// 3. Build a fresh turnRunner for this turn.
	rctx := RuntimeContext{
		Session:      sess,
		Publisher:    pub,
		Approver:     e.active.Approver(sess.ID),
		PlanApprover: e.active.PlanApprover(sess.ID),
		ClientWaiter: e.active.ClientToolWaiter(sess.ID),
		ClientTools:  e.active.ClientTools(sess.ID),
		// Mid-turn crash-safety (v1.2 §2): persist at each loop boundary so a hard
		// kill loses at most the in-progress step. The turn-boundary Save below is
		// still the backstop.
		Checkpointer: repoCheckpointer{repo: e.repo, onErr: e.OnSaveError},
	}
	runner := e.rb.Build(rctx)

	// 4. Run the turn.
	res, runErr := runner.RunTurn(turnCtx, sess, input)

	// 5. Save — always, even on error/cancel. RunTurn appends the user message
	//    before its first cancellation checkpoint, so partial history is consistent
	//    and resumable. WithoutCancel keeps the save from being aborted by the
	//    turn's (or the caller's) cancellation.
	if !sess.IsEmpty() {
		if serr := e.repo.Save(context.WithoutCancel(parentCtx), sess); serr != nil && e.OnSaveError != nil {
			e.OnSaveError(serr)
		}
	}

	// 6. Auto-name: set an initial truncation-based name on the first turn, then
	//    fire async LLM title generation if configured.
	if sess.Name == "" {
		initial := truncateForTitle(firstUserMessage(sess))
		if initial != "" {
			sess.Name = initial
			// Persist the initial name (best-effort, fire-and-forget).
			go func() {
				_ = e.repo.UpdateName(context.WithoutCancel(parentCtx), sess.ID, initial)
			}()
		}
	}
	if e.titleGen != nil && turnCount(sess) == 1 {
		go e.generateTitleAsync(sess)
	}

	return res, runErr
}

// Cancel stops the in-flight turn for a session at the next checkpoint.
func (e *TurnExecutor) Cancel(sessionID string) {
	e.active.Cancel(sessionID)
}

// repoCheckpointer persists the session mid-turn via the repository (v1.2 §2). It
// detaches cancellation with WithoutCancel so a checkpoint fired as the turn is
// being suspended still commits, and skips an empty session (nothing to persist
// yet). Best-effort: it reports a failure through onErr but never blocks the turn —
// the turn-boundary Save in ExecuteWithSession is the authoritative write.
type repoCheckpointer struct {
	repo  ConversationRepository
	onErr func(error)
}

func (c repoCheckpointer) Checkpoint(ctx context.Context, sess *session.Session) error {
	if sess.IsEmpty() {
		return nil
	}
	err := c.repo.Save(context.WithoutCancel(ctx), sess)
	if err != nil && c.onErr != nil {
		c.onErr(err)
	}
	return err
}

// SetApprover associates an approver with a session.
func (e *TurnExecutor) SetApprover(sessionID string, a agent.Approver) {
	e.active.SetApprover(sessionID, a)
}

// SetPlanApprover associates a plan approver with a session.
func (e *TurnExecutor) SetPlanApprover(sessionID string, pa agent.PlanApprover) {
	e.active.SetPlanApprover(sessionID, pa)
}

// SetClientToolWaiter associates a client tool waiter with a session.
func (e *TurnExecutor) SetClientToolWaiter(sessionID string, w agent.ClientToolWaiter) {
	e.active.SetClientToolWaiter(sessionID, w)
}

// RegisterTools stores client-side tool definitions for a session.
func (e *TurnExecutor) RegisterTools(sessionID string, tools []agent.ClientToolDef) {
	e.active.RegisterTools(sessionID, tools)
}

// Shutdown cancels all active turns and closes event buses.
func (e *TurnExecutor) Shutdown() {
	e.active.Shutdown()
	e.subs.Shutdown()
}

// ---- auto-naming helpers ----

// firstUserMessage returns the content of the first user message in the session.
func firstUserMessage(sess *session.Session) string {
	for _, m := range sess.Messages {
		if m.Role == model.RoleUser {
			return m.Content
		}
	}
	return ""
}

// truncateForTitle collapses whitespace and truncates msg to a reasonable
// display width for use as a fallback session title.
func truncateForTitle(msg string) string {
	msg = strings.TrimSpace(msg)
	// First line only.
	if idx := strings.IndexAny(msg, "\r\n"); idx >= 0 {
		msg = msg[:idx]
	}
	// Collapse whitespace.
	msg = strings.Join(strings.Fields(msg), " ")
	const maxLen = 60
	if len(msg) > maxLen {
		msg = msg[:maxLen]
	}
	return strings.TrimSpace(msg)
}

// turnCount returns the number of user turns in the session.
func turnCount(sess *session.Session) int {
	n := 0
	for _, m := range sess.Messages {
		if m.Role == model.RoleUser {
			n++
		}
	}
	return n
}

// generateTitleAsync runs the LLM title generator in a background goroutine.
// It uses a detached context with a timeout so it is not tied to the turn's
// lifecycle. Best-effort: failures are silently ignored.
func (e *TurnExecutor) generateTitleAsync(sess *session.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	userMsg := firstUserMessage(sess)
	assistantMsg := ""
	for _, m := range sess.Messages {
		if m.Role == model.RoleAssistant && m.Content != "" {
			assistantMsg = m.Content
			break
		}
	}

	title, err := e.titleGen.GenerateTitle(ctx, userMsg, assistantMsg)
	if err != nil || title == "" {
		return
	}
	_ = e.repo.UpdateName(ctx, sess.ID, title)
}

// ---- internal emitters ----

// compositeEmitter fans one event to multiple sinks. If a sink panics, the
// next sink still receives the event (best-effort per sink).
type compositeEmitter []agent.Emitter

func (c compositeEmitter) Emit(e agent.Event) {
	for _, s := range c {
		s.Emit(e)
	}
}

// eventStoreEmitter persists each event to the ConversationEventStore. It skips
// ephemeral token-delta events (too frequent to persist usefully).
type eventStoreEmitter struct {
	ctx    context.Context
	events ConversationEventStore
}

func (e *eventStoreEmitter) Emit(ev agent.Event) {
	if ev.Kind == agent.EventTokenDelta {
		return // ephemeral, not persisted
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return // best-effort: a marshal error is not actionable
	}
	_ = e.events.Append(e.ctx, session.EventRecord{
		SessionID: ev.SessionID,
		TurnID:    ev.TurnID,
		Kind:      string(ev.Kind),
		At:        ev.At,
		Payload:   payload,
	})
}
