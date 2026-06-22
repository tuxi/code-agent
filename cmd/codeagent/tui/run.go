// Package tui is the BubbleTea "agent workspace" renderer (Phase 7, M1). It is a
// second consumer of the runtime's existing event stream and Approver interface —
// it adds no agent capability and the agent never learns it exists. The loop runs
// on a background goroutine; events and approvals cross to the render loop over
// channels (see backend.go and docs/p7-tui.md §5).
package tui

import (
	"context"
	"encoding/json"
	"errors"

	"code-agent/internal/agent"
	"code-agent/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// Store is the slice of the session store the TUI needs: persist after each turn,
// list saved sessions (for /sessions and the /resume picker), and read a
// session's persisted events (to replay its history on resume).
type Store interface {
	Save(ctx context.Context, s *session.Session) error
	List(ctx context.Context) ([]session.Meta, error)
	SessionEvents(ctx context.Context, id string) ([]session.EventRecord, error)
}

// ResumeFunc loads a saved session by id and re-budgets it to the current model
// (provided by the cmd layer, which owns the config). Used by /resume.
type ResumeFunc func(id string) (*session.Session, error)

// AutoMode is the cmd layer's auto-approval toggle, surfaced to the /auto command.
// Satisfied by *approve.AutoApprover; kept as an interface so the tui package does
// not import the approve package.
type AutoMode interface {
	Enabled() bool
	SetEnabled(bool)
}

// PursueFunc runs a /goal pursuit on the active session to a terminal state and
// returns a one-line outcome summary. It is injected by the cmd layer, which owns
// admission and engine/checker construction; the run loop calls it like a long
// turn (events and approval cards flow through the existing channels). A nil
// PursueFunc disables /goal in the TUI.
type PursueFunc func(ctx context.Context, sess *session.Session, objective string) (summary string, err error)

// ModelSwapFunc switches the runner to a new model by name, rebuilding the
// provider/compactor and re-budgeting the session. Called inside the run-loop
// goroutine between turns (the same select safety as /resume). Returns the
// updated header info for the TUI, or an error.
type ModelSwapFunc func(name string) (HeaderInfo, error)

// modelInfo is one selectable model for the /use picker.
type modelInfo struct {
	name string
}

// sessionSource is the cmd-layer's window into saved sessions for /sessions and
// /resume: list them, load+rebudget one, and replay one's events.
type sessionSource struct {
	list   func() []session.Meta
	resume ResumeFunc
	events func(id string) []agent.Event

	modelNames   []modelInfo          // for the /use picker
	modelSwap    ModelSwapFunc        // switches the model between turns
	modelSwapped chan modelSwappedMsg // the TUI awaits this after posting a model name

	auto AutoMode // /auto toggle (nil if not wired)
}

// Run starts the workspace. The runner must already be wired to b.Emitter and
// b.Approver (via buildRunner). One background goroutine pulls a prompt off
// b.inputs, runs the turn, persists, and signals done — and handles
// session/model swaps between turns. The BubbleTea program owns the terminal.
// Run blocks until the user quits.
func Run(ctx context.Context, b *Backend, runner *agent.Runner, sess *session.Session, store Store, header HeaderInfo, resume ResumeFunc, modelSwap ModelSwapFunc, modelNames []string, auto AutoMode, pursue PursueFunc) error {
	// Cancelling on quit stops an in-flight turn rather than orphaning it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		for {
			select {
			case input, ok := <-b.inputs:
				if !ok {
					return
				}
				turnCtx, cancel := context.WithCancel(ctx)
				b.mu.Lock()
				b.turnCancel = cancel
				b.mu.Unlock()
				_, err := runner.RunTurn(turnCtx, sess, input)
				cancel() // always clean up
				b.mu.Lock()
				b.turnCancel = nil
				b.mu.Unlock()
				// Persist whatever the turn produced, even on error (including
				// context.Canceled from ctrl+c): the partial history is consistent
				// and resumable (same contract as run/repl).
				if serr := store.Save(ctx, sess); serr != nil && err == nil {
					err = serr
				}
				b.done <- err
			case ns := <-b.sessSwap:
				// /resume swaps the active session between turns. The select can't
				// fire mid-RunTurn, so a swap always lands at a turn boundary — no
				// hot-swap of a running turn. Persist the outgoing session first.
				if !sess.IsEmpty() {
					_ = store.Save(ctx, sess)
				}
				sess = ns
			case name := <-b.modelSwap:
				h, err := modelSwap(name)
				b.modelSwapResult <- modelSwappedMsg{header: h, err: err}
			case on := <-b.planToggle:
				// Applied at a turn boundary (the select can't fire mid-RunTurn), so
				// the next turn runs in the chosen mode — no hot-swap of a live turn.
				runner.PlanMode = on
			case obj := <-b.goalStart:
				// A /goal pursuit is a long, multi-turn turn: it drives runner.RunTurn
				// repeatedly via the injected engine, so events and approval cards flow
				// through the same channels. turnCancel cancels the whole pursuit, so
				// ctrl+c pauses it (the engine settles to paused). The engine is the sole
				// session writer, so no Save here.
				var summary string
				var err error
				if pursue == nil {
					err = errors.New("/goal is not available in this session")
				} else {
					turnCtx, cancel := context.WithCancel(ctx)
					b.mu.Lock()
					b.turnCancel = cancel
					b.mu.Unlock()
					summary, err = pursue(turnCtx, sess, obj)
					cancel()
					b.mu.Lock()
					b.turnCancel = nil
					b.mu.Unlock()
				}
				b.goalDone <- goalDoneMsg{summary: summary, err: err}
			}
		}
	}()

	models := make([]modelInfo, len(modelNames))
	for i, n := range modelNames {
		models[i] = modelInfo{name: n}
	}
	src := sessionSource{
		list: func() []session.Meta {
			metas, err := store.List(ctx)
			if err != nil {
				return nil
			}
			return metas
		},
		resume: resume,
		events: func(id string) []agent.Event {
			recs, err := store.SessionEvents(ctx, id)
			if err != nil {
				return nil
			}
			out := make([]agent.Event, 0, len(recs))
			for _, r := range recs {
				var ev agent.Event
				if json.Unmarshal(r.Payload, &ev) == nil {
					out = append(out, ev)
				}
			}
			return out
		},
		modelNames:   models,
		modelSwap:    modelSwap,
		modelSwapped: b.modelSwapResult,
		auto:         auto,
	}
	// Inline mode (no alt-screen, no mouse capture): finalized output goes to the
	// terminal's own scrollback, so native select/copy, scroll, and Ctrl+R search
	// all just work. The program only owns the live region (status + composer).
	p := tea.NewProgram(
		newModel(b, header, src),
		tea.WithContext(ctx),
	)
	_, err := p.Run()
	close(b.inputs) // stop the turn loop; in-flight turn is cancelled via ctx
	return err
}
